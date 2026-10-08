package database

import (
	"context"
	"database/sql"

	"pad-core/logger"

	"pad-analyzer/internal/storage/interfaces"
)

// The organisations domain: the organisation, its members and invitations, its shared credentials, and the
// configuration and content it owns (policies, channels, custom rules, the knowledge base).
func init() {
	const orgConfig = "organisation configuration, no user identity"
	registerLifecycle(lifecycleDomain{
		name: "organisations",
		tables: []tableClaim{
			{table: "organisations", export: "organisations", erase: erasedByStep, org: orgByStep,
				noPurge: "an organisation lives until an admin deletes it"},
			{table: "org_members", export: "organisations", erase: erasedByStep, org: orgByCascade,
				noPurge: "a membership lives until it is removed"},
			{table: "org_invites", noExport: "invitations to the user's email (open: not in the bundle)",
				erase: erasedByStep, org: orgByCascade},
			{table: "org_provider_keys", noExport: "the organisation's encrypted shared credentials",
				erase: erasedByStep, org: orgByCascade, noPurge: "a credential lives until an admin removes it"},
			{table: "policies", noExport: orgConfig, erase: notPersonal, eraseWhy: orgConfig, org: orgByStep,
				noPurge: "configuration lives as long as its organisation"},
			{table: "org_channels", noExport: orgConfig, erase: notPersonal, eraseWhy: orgConfig, org: orgByStep,
				noPurge: "configuration lives as long as its organisation"},
			{table: "org_custom_rules", noExport: orgConfig, erase: notPersonal, eraseWhy: orgConfig, org: orgByStep,
				noPurge: "configuration lives as long as its organisation"},
			{table: "knowledge_documents", noExport: "the organisation's knowledge base",
				erase: notPersonal, eraseWhy: "organisation content; no uploader is recorded", org: orgByStep,
				noPurge: "organisation content, never on a timer"},
			{table: "knowledge_chunks", noExport: "the organisation's knowledge base",
				erase: notPersonal, eraseWhy: "organisation content; no uploader is recorded", org: orgByCascade,
				noPurge: "organisation content, never on a timer"},
		},
		export: []exportSection{
			section("organisations", exportOrganisations,
				func(out *interfaces.UserDataExport, v []*interfaces.Organisation) { out.Organisations = v }),
		},
		erase: []eraseStep{
			{phase: eraseIdentity, table: "organisations", run: eraseOrgOwnership},
			eraseByUser(eraseIdentity, "org_invites", `UPDATE org_invites SET invited_by = '' WHERE invited_by = $1`),
			// The credentials the user lent (secret_set_by, or updated_by on a row no save has stamped since v24) are
			// AccountLifecycleService's: its finalize disables them by the user's id after this statement, on another
			// connection, before the request commits. Touching those rows here would make it wait on this
			// transaction's lock, and the user's id would no longer find them.
			eraseByUser(eraseIdentity, "org_provider_keys", `UPDATE org_provider_keys SET updated_by = ''
		WHERE updated_by = $1 AND secret_set_by <> '' AND secret_set_by <> $1`),
			eraseByUser(erasePersonal, "org_members", `DELETE FROM org_members WHERE user_id = $1`),
			eraseByEmail(erasePersonal, "org_invites", `DELETE FROM org_invites WHERE email = $1`),
		},
		orgDelete: []orgDeleteStep{
			// No foreign key reaches these from organisations; knowledge_chunks cascades with its document.
			{phase: orgContent, table: "knowledge_documents", sql: `DELETE FROM knowledge_documents WHERE org_id = $1`},
			{phase: orgContent, table: "policies", sql: `DELETE FROM policies WHERE org_id = $1`},
			{phase: orgContent, table: "org_channels", sql: `DELETE FROM org_channels WHERE org_id = $1`},
			{phase: orgContent, table: "org_custom_rules", sql: `DELETE FROM org_custom_rules WHERE org_id = $1`},
			{phase: orgRow, table: "organisations", sql: `DELETE FROM organisations WHERE id = $1`},
		},
		purge: []purgeStep{
			// Invites that expired, or were accepted more than 7 days ago (kept briefly so the accept event and the
			// audit can correlate).
			{table: "org_invites", cond: `expires_at < NOW() OR (accepted_at IS NOT NULL AND accepted_at < NOW() - INTERVAL '7 days')`,
				count: func(r *interfaces.PurgeResult) *int { return &r.OrgInvites }},
		},
	})
}

// exportOrganisations returns the organisations the user belongs to. Each carries the user's own membership only:
// the other members' ids and roles are their data, not the user's.
func exportOrganisations(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.Organisation, error) {
	orgs, err := b.ListOrgsForUser(ctx, x.userID)
	if err != nil {
		return nil, err
	}
	out := make([]*interfaces.Organisation, 0, len(orgs))
	for _, o := range orgs {
		own := []interfaces.OrgMember{}
		for _, m := range o.Members {
			if m.UserID == x.userID {
				own = append(own, m)
			}
		}
		o.Members = own
		out = append(out, o)
	}
	return out, nil
}

// eraseOrgOwnership clears the user as the owner of their organisations. An organisation stays with its members and
// content when the account goes, even one the user was the last admin of: nobody can administer or delete it then,
// so the erasure names those organisations in the log for an operator (whether erasure should refuse, hand the
// organisation over or delete it is an open product decision).
func eraseOrgOwnership(ctx context.Context, _ *PostgresStorageBackend, tx *sql.Tx, e *erasure) error {
	orphaned, err := rowsOfStrings(ctx, tx, `SELECT m.org_id FROM org_members m
		WHERE m.user_id = $1 AND m.role = 'admin'
		  AND NOT EXISTS (SELECT 1 FROM org_members o WHERE o.org_id = m.org_id AND o.user_id <> $1 AND o.role = 'admin')
		ORDER BY m.org_id`, e.userID)
	if err != nil {
		return err
	}
	if len(orphaned) > 0 {
		e.after = append(e.after, func(ctx context.Context) {
			logger.FromContext(ctx).Warn("account erasure left organisations without an admin", "org_ids", orphaned)
		})
	}
	_, err = tx.ExecContext(ctx, `UPDATE organisations SET owner_id = '' WHERE owner_id = $1`, e.userID)
	return err
}
