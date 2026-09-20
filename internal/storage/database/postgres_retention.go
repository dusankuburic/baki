package database

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"pad-core/logger"

	"pad-analyzer/internal/storage/interfaces"
)

// DeleteUser performs GDPR-style account erasure for userID. It removes the
// user's owned flows (cascading versions/analysis/triage/baselines/conversations)
// and all per-user rows (tokens, settings, usage, org memberships, invites for
// their email), and anonymizes personal data they authored on shared rows that
// must be retained for security/forensic integrity (audit_events email/IP,
// flow_versions/finding_status/flow_baselines created_by/updated_by).
//
// Erasure runs in its own transaction WITHOUT setting app.current_user_id, so
// Row-Level Security is bypassed (app_rls_active()=false → allow-all). This is
// correct for a maintenance/erasure path that must touch rows across all of the
// user's flows — including those they only collaborated on — while the Go layer
// has already authenticated the caller. Idempotent: a missing user is not an
// error.
func (b *PostgresStorageBackend) DeleteUser(ctx context.Context, userID string) error {
	var ownedFlowIDs []string
	if err := b.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		ownedFlowIDs, err = b.deleteUserInTx(ctx, tx, userID)
		return err
	}); err != nil {
		return err
	}

	// Best-effort blob cleanup for the flows that were just removed. Deferred
	// via RegisterPostCommit rather than run inline: when this request carries
	// the middleware's RLS transaction, withTx JOINS it and nothing is durable
	// until the middleware commits, so deleting blobs here would orphan content
	// belonging to rows that a rollback puts back. RegisterPostCommit runs fn
	// inline when there is no registry (autocommit -- the write is already
	// durable), so the standalone path is unchanged.
	if b.blobClient != nil {
		for _, fid := range ownedFlowIDs {
			flowID := fid
			RegisterPostCommit(ctx, func() {
				b.scheduleBlobCleanup(time.Now(), "delete-user-flow:"+flowID, func(ctx context.Context) {
					if err := b.deleteBlobs(ctx, fmt.Sprintf("flows/%s/", flowID)); err != nil {
						logger.Warn("DeleteUser: failed to delete flow blobs", "flow_id", flowID, "error", err)
					}
				})
			})
		}
	}
	return nil
}

// deleteUserInTx performs the erasure inside tx and returns the IDs of the
// flows it removed, so the caller can schedule blob cleanup after the commit.
func (b *PostgresStorageBackend) deleteUserInTx(ctx context.Context, tx *sql.Tx, userID string) (ownedFlowIDs []string, err error) {
	// Capture email + owned flow IDs before deleting: org_invites is keyed by
	// email (not user_id), and owned flows' blob content must be cleaned up.
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		// B1.8: a TRANSIENT scan error used to silently skip the org_invites
		// cleanup keyed by email — a gap in an audited GDPR flow, so it must
		// propagate. But "no rows" is not transient: it is the documented
		// idempotent case (the user is already erased, or never existed).
		// Treating it as an error broke DeleteUser's idempotency contract.
		// The email is simply unknown here, which the `if email != ""` guard
		// on the org_invites delete below already handles — so fall through
		// and let the remaining per-user sweeps run, which cleans up any
		// residue a partial prior erasure left behind.
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("erasure: read user email: %w", err)
		}
	}

	ownedFlowIDs, err = rowsOfStrings(ctx, tx, `SELECT id FROM flows WHERE owner_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("erasure: list owned flows: %w", err)
	}

	// Owned flows: ON DELETE CASCADE removes their versions, analysis history,
	// triage, baselines, conversations and collaborator rows.
	if _, err = tx.ExecContext(ctx, `DELETE FROM flows WHERE owner_id = $1`, userID); err != nil {
		return nil, fmt.Errorf("delete user flows: %w", err)
	}

	// Anonymize PII on surviving rows. The events/versions are kept for
	// forensic integrity; only personal data is stripped.
	if _, err = tx.ExecContext(ctx, `UPDATE audit_events SET email = '', ip = '', user_id = '' WHERE user_id = $1`, userID); err != nil {
		return nil, fmt.Errorf("anonymize audit_events: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flow_versions SET created_by = '' WHERE created_by = $1`, userID); err != nil {
		return nil, fmt.Errorf("anonymize flow_versions: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE finding_status SET updated_by = '' WHERE updated_by = $1`, userID); err != nil {
		return nil, fmt.Errorf("anonymize finding_status: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flow_baselines SET created_by = '' WHERE created_by = $1`, userID); err != nil {
		return nil, fmt.Errorf("anonymize flow_baselines: %w", err)
	}

	// Per-user rows that have no ON DELETE CASCADE foreign key.
	for _, stmt := range []string{
		`DELETE FROM refresh_tokens WHERE user_id = $1`,
		`DELETE FROM usage_metrics WHERE user_id = $1`,
		`DELETE FROM provider_keys WHERE user_id = $1`,
		`DELETE FROM org_members WHERE user_id = $1`,
	} {
		if _, err = tx.ExecContext(ctx, stmt, userID); err != nil {
			return nil, fmt.Errorf("delete per-user rows: %w", err)
		}
	}
	if email != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM org_invites WHERE email = $1`, email); err != nil {
			return nil, fmt.Errorf("delete org_invites: %w", err)
		}
	}

	// The user row last; api_tokens, user_settings, identity_links and
	// flow_collaborators cascade via their ON DELETE CASCADE foreign keys.
	if _, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return nil, fmt.Errorf("delete user: %w", err)
	}
	// The commit belongs to withTx, which may be joining the request's RLS
	// transaction rather than owning one.
	return ownedFlowIDs, nil
}

// ExportUserData assembles the user's personal data for a data-subject access /
// portability request: profile, owned flows, settings, API tokens, and audit
// history. Reads use the caller's request context (RLS-scoped to the user for
// their own data).
func (b *PostgresStorageBackend) ExportUserData(ctx context.Context, userID string) (*interfaces.UserDataExport, error) {
	user, err := b.LoadUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Page through ALL of the user's flows: ListFlows clamps Limit<=0 to a
	// default page size, so a single call would silently truncate the export
	// for users with more flows than one page — unacceptable for a
	// data-subject access request, which must be complete.
	//
	// FlowSortIDAsc, not the default: this walk pages with LIMIT/OFFSET, and
	// the default ordering is on updated_at — a MUTABLE column. A flow saved
	// while the export runs (the user in another tab, or a collaborator on a
	// shared flow) jumps to the front of that ordering and pushes the row on
	// the next page boundary out of the walk entirely. The completeness guards
	// below then run only over the flows that survived it, so the export would
	// pass every check it makes and still be missing data. Ordering on the
	// immutable primary key means an update cannot move a row.
	const exportPageSize = 500 // ListFlows' maximum page size
	var flows []*interfaces.FlowDocument
	for offset := 0; ; offset += exportPageSize {
		page, err := b.ListFlows(ctx, interfaces.FlowFilter{
			UserID: userID, SortBy: interfaces.FlowSortIDAsc,
			Limit: exportPageSize, Offset: offset,
		})
		if err != nil {
			return nil, fmt.Errorf("export: list flows: %w", err)
		}
		flows = append(flows, page...)
		if len(page) < exportPageSize {
			break
		}
	}
	// A data-subject export must be complete or fail. Transient blob failures
	// already fail ListFlows; this catches the remaining case — a flow whose
	// content blob is permanently missing (ListFlows returns it with nil
	// content) — instead of silently shipping an export without its content.
	for _, f := range flows {
		if len(f.Content) == 0 && metadataRecordsContent(f.Metadata) {
			return nil, fmt.Errorf("export: content for flow %s is unavailable; refusing to produce an incomplete export", f.ID)
		}
	}
	settings, _ := b.LoadUserSettings(ctx, userID)
	tokens, err := b.ListAPITokens(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: list tokens: %w", err)
	}
	audit, err := b.exportAuditEvents(ctx, userID)
	if err != nil {
		return nil, err
	}
	flowIDs := make([]string, 0, len(flows))
	for _, f := range flows {
		flowIDs = append(flowIDs, f.ID)
	}
	sections, err := b.exportUserSections(ctx, userID, flowIDs)
	if err != nil {
		return nil, err
	}
	return &interfaces.UserDataExport{
		User:            user,
		Flows:           flows,
		Settings:        settings,
		AuditEvents:     interfaces.NewExportedAuditEvents(audit),
		APITokens:       tokens,
		Conversations:   sections.conversations,
		Comments:        sections.comments,
		TriageDecisions: sections.triage,
		Organisations:   sections.orgs,
		Sessions:        sections.sessions,
		ExportedAt:      time.Now().UTC(),
	}, nil
}

// exportAuditEvents pages through the user's whole audit history. A single
// Limit:1000 call silently truncated the trail for active accounts with no
// indication in the bundle.
func (b *PostgresStorageBackend) exportAuditEvents(ctx context.Context, userID string) ([]*interfaces.AuditEvent, error) {
	const auditPageSize = 1000
	audit := []*interfaces.AuditEvent{}
	for offset := 0; ; offset += auditPageSize {
		page, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: userID, Limit: auditPageSize, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("export: list audit: %w", err)
		}
		audit = append(audit, page...)
		if len(page) < auditPageSize {
			return audit, nil
		}
	}
}

// userExportSections are the bundle sections that follow the flows: chat
// history on the user's own flows, the comments and triage decisions they
// authored, their organisations and their active sessions.
type userExportSections struct {
	conversations []*interfaces.ExportedConversation
	comments      []*interfaces.FindingComment
	triage        []*interfaces.FindingStatus
	orgs          []*interfaces.Organisation
	sessions      []interfaces.RefreshTokenInfo
}

// exportUserSections loads userExportSections for both export paths, so the
// buffered and the streamed bundle cannot drift apart again. (They did: the
// sections were added to ExportUserData only, while the account-export handler
// always streams on Postgres, so users never received them.) flowIDs are the
// user's owned flows, which scope conversations and triage.
func (b *PostgresStorageBackend) exportUserSections(ctx context.Context, userID string, flowIDs []string) (*userExportSections, error) {
	var out userExportSections
	var err error
	if out.conversations, err = b.exportConversations(ctx, flowIDs); err != nil {
		return nil, fmt.Errorf("export: conversations: %w", err)
	}
	if out.comments, err = b.exportAuthoredComments(ctx, userID); err != nil {
		return nil, fmt.Errorf("export: comments: %w", err)
	}
	if out.triage, err = b.exportTriageDecisions(ctx, userID, flowIDs); err != nil {
		return nil, fmt.Errorf("export: triage: %w", err)
	}
	if out.orgs, err = b.ListOrgsForUser(ctx, userID); err != nil {
		return nil, fmt.Errorf("export: organisations: %w", err)
	}
	if out.sessions, err = b.ListUserRefreshTokens(ctx, userID); err != nil {
		return nil, fmt.Errorf("export: sessions: %w", err)
	}
	// Empty sections serialise as [] rather than null, so a consumer of the
	// bundle can tell "none" from "section missing".
	if out.orgs == nil {
		out.orgs = []*interfaces.Organisation{}
	}
	if out.sessions == nil {
		out.sessions = []interfaces.RefreshTokenInfo{}
	}
	return &out, nil
}

// exportConversations returns every stored chat conversation on the given
// (owned) flows. Chat with the assistant about your own flow is user-authored
// content and belongs in the bundle.
func (b *PostgresStorageBackend) exportConversations(ctx context.Context, flowIDs []string) ([]*interfaces.ExportedConversation, error) {
	out := []*interfaces.ExportedConversation{}
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT flow_id, scope, messages FROM conversations WHERE flow_id = ANY($1) ORDER BY flow_id, scope`,
		flowIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c interfaces.ExportedConversation
		var data []byte
		if err := rows.Scan(&c.FlowID, &c.Scope, &data); err != nil {
			return nil, err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &c.Messages); err != nil {
				return nil, fmt.Errorf("unmarshal conversation %s/%s: %w", c.FlowID, c.Scope, err)
			}
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// exportAuthoredComments returns the comments the user wrote, on any flow —
// they are the user's own words even when the flow belongs to someone else.
func (b *PostgresStorageBackend) exportAuthoredComments(ctx context.Context, userID string) ([]*interfaces.FindingComment, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, flow_id, finding_key, author_id, author_name, body, created_at
		 FROM finding_comments WHERE author_id = $1 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*interfaces.FindingComment{}
	for rows.Next() {
		c := &interfaces.FindingComment{}
		if err := rows.Scan(&c.ID, &c.FlowID, &c.FindingKey, &c.AuthorID, &c.AuthorName, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// exportTriageDecisions returns the triage rows the user last updated on the
// given (owned) flows — status changes, assignments and justifications they
// recorded. Rows on those flows last touched by a collaborator are that
// collaborator's content and stay out.
func (b *PostgresStorageBackend) exportTriageDecisions(ctx context.Context, userID string, flowIDs []string) ([]*interfaces.FindingStatus, error) {
	out := []*interfaces.FindingStatus{}
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT flow_id, finding_key, rule_id, status, justification, assignee_id, updated_by, updated_at
		FROM finding_status WHERE flow_id = ANY($1) AND updated_by = $2 ORDER BY flow_id, finding_key`,
		flowIDs, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st interfaces.FindingStatus
		if err := rows.Scan(&st.FlowID, &st.FindingKey, &st.RuleID, &st.Status, &st.Justification, &st.AssigneeID, &st.UpdatedBy, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

// exportFlowPageSize bounds how many flows (with content) ExportUserDataTo
// holds in memory at once. Smaller than ListFlows' max so the streaming export
// keeps peak memory to one modest page rather than every flow's content.
const exportFlowPageSize = 100

// ExportUserDataTo streams the same JSON bundle as ExportUserData to w, but
// fetches flows one page at a time and releases each page before the next, so
// peak memory is bounded to a single page's content instead of every flow's
// content at once. Callers MUST treat w as tainted if a non-nil error is
// returned (partial JSON may have been written) — the HTTP handler streams to a
// temp file and only forwards it to the client on success, preserving the
// "complete or fail" guarantee a data-subject export requires.
func (b *PostgresStorageBackend) ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error {
	user, err := b.LoadUserByID(ctx, userID)
	if err != nil {
		return err
	}
	settings, _ := b.LoadUserSettings(ctx, userID)
	tokens, err := b.ListAPITokens(ctx, userID)
	if err != nil {
		return fmt.Errorf("export: list tokens: %w", err)
	}
	audit, err := b.exportAuditEvents(ctx, userID)
	if err != nil {
		return err
	}

	bw := bufio.NewWriter(w)
	writeField := func(prefix string, v any) error {
		if _, err := bw.WriteString(prefix); err != nil {
			return err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = bw.Write(data)
		return err
	}

	// Object field order mirrors interfaces.UserDataExport's json tags so the
	// streamed bytes are byte-shape-compatible with the buffered encoder.
	if err := writeField(`{"user":`, user); err != nil {
		return err
	}
	if _, err := bw.WriteString(`,"flows":[`); err != nil {
		return err
	}
	first := true
	var flowIDs []string // owned flows, which scope the conversation and triage sections
	// Immutable ordering, for the same reason as ExportUserData above: a
	// concurrent save must not be able to drop a flow from a data-subject
	// export.
	for offset := 0; ; offset += exportFlowPageSize {
		page, err := b.ListFlows(ctx, interfaces.FlowFilter{
			UserID: userID, SortBy: interfaces.FlowSortIDAsc,
			Limit: exportFlowPageSize, Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("export: list flows: %w", err)
		}
		for _, f := range page {
			// Same completeness guard as ExportUserData: a permanently missing
			// content blob must fail the export, not ship an empty flow.
			if len(f.Content) == 0 && metadataRecordsContent(f.Metadata) {
				return fmt.Errorf("export: content for flow %s is unavailable; refusing to produce an incomplete export", f.ID)
			}
			if !first {
				if _, err := bw.WriteString(","); err != nil {
					return err
				}
			}
			first = false
			flowIDs = append(flowIDs, f.ID)
			data, err := json.Marshal(f)
			if err != nil {
				return fmt.Errorf("export: marshal flow %s: %w", f.ID, err)
			}
			if _, err := bw.Write(data); err != nil {
				return err
			}
		}
		if len(page) < exportFlowPageSize {
			break
		}
	}
	if _, err := bw.WriteString(`]`); err != nil {
		return err
	}
	if err := writeField(`,"settings":`, settings); err != nil {
		return err
	}
	if err := writeField(`,"auditEvents":`, interfaces.NewExportedAuditEvents(audit)); err != nil {
		return err
	}
	if err := writeField(`,"apiTokens":`, tokens); err != nil {
		return err
	}
	sections, err := b.exportUserSections(ctx, userID, flowIDs)
	if err != nil {
		return err
	}
	for _, field := range []struct {
		prefix string
		value  any
	}{
		{`,"conversations":`, sections.conversations},
		{`,"comments":`, sections.comments},
		{`,"triageDecisions":`, sections.triage},
		{`,"organisations":`, sections.orgs},
		{`,"sessions":`, sections.sessions},
	} {
		if err := writeField(field.prefix, field.value); err != nil {
			return err
		}
	}
	if err := writeField(`,"exportedAt":`, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := bw.WriteString("}"); err != nil {
		return err
	}
	return bw.Flush()
}

// PurgeExpiredData removes stale rows whose retention has elapsed. It is
// intended for a periodic background job (single-replica: run on the one
// instance). auditRetentionDays <= 0 keeps audit_events indefinitely (only
// expired tokens/invites are purged). Returns counts of rows removed.
func (b *PostgresStorageBackend) PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*interfaces.PurgeResult, error) {
	out := &interfaces.PurgeResult{}

	stmts := []struct {
		sql  string
		dest *int
	}{
		{`DELETE FROM refresh_tokens WHERE expires_at < NOW()`, &out.RefreshTokens},
		{`DELETE FROM api_tokens WHERE expires_at IS NOT NULL AND expires_at < NOW()`, &out.APITokens},
		{`DELETE FROM user_tokens WHERE expires_at < NOW()`, &out.UserTokens},
		// Purge invites that expired, or were accepted more than 7 days ago
		// (kept briefly so the accept event / audit can correlate).
		{`DELETE FROM org_invites WHERE expires_at < NOW() OR (accepted_at IS NOT NULL AND accepted_at < NOW() - INTERVAL '7 days')`, &out.OrgInvites},
		// Expired blacklist entries are pure dead weight (the token they
		// reference has already expired), so purge them unconditionally —
		// previously this was only done by a per-process goroutine that could
		// die or be absent on some replicas, letting the table grow unbounded.
		{`DELETE FROM token_blacklist WHERE expires_at < NOW()`, &out.TokenBlacklist},
		// An expired share link can never be redeemed again (the lookup
		// checks expires_at), so its row is dead weight — and one that still
		// names a flow. Rows with no expiry are permanent links; kept.
		{`DELETE FROM share_tokens WHERE expires_at IS NOT NULL AND expires_at < NOW()`, &out.ShareTokens},
	}
	// Deliberately NOT purged by age: conversations, knowledge documents and
	// finding comments are user content, not telemetry. They go when their
	// flow/org goes (ON DELETE CASCADE), never on a timer.
	for _, s := range stmts {
		res, err := b.db.ExecContext(ctx, s.sql)
		if err != nil {
			return out, fmt.Errorf("purge: %w", err)
		}
		n, _ := res.RowsAffected()
		*s.dest = int(n)
	}

	if auditRetentionDays > 0 {
		aged := []struct {
			sql  string
			dest *int
		}{
			{`DELETE FROM audit_events WHERE created_at < NOW() - ($1 * INTERVAL '1 day')`, &out.AuditEvents},
			// flow_analysis_history is append-only (one row per analyze run)
			// and the dashboard only reads the last 14–30 days, so age it out
			// alongside audit data instead of letting it grow forever.
			{`DELETE FROM flow_analysis_history WHERE analyzed_at < NOW() - ($1 * INTERVAL '1 day')`, &out.FlowAnalysisHistory},
			// usage_metrics is append-only (one row per AI call) and drives
			// the same dashboard's cost/trend views; age it out on the same
			// retention window.
			{`DELETE FROM usage_metrics WHERE created_at < NOW() - ($1 * INTERVAL '1 day')`, &out.UsageMetrics},
			// gov_alerts is one row per alert per sweep, forever — the
			// scanner's in-memory dedup does not read the table back, so
			// ageing rows out cannot cause a re-alert. Same window as audit.
			{`DELETE FROM gov_alerts WHERE created_at < NOW() - ($1 * INTERVAL '1 day')`, &out.GovAlerts},
		}
		for _, s := range aged {
			res, err := b.db.ExecContext(ctx, s.sql, auditRetentionDays)
			if err != nil {
				return out, fmt.Errorf("purge: %w", err)
			}
			n, _ := res.RowsAffected()
			*s.dest = int(n)
		}
	}
	return out, nil
}

// rowsOfStrings reads the first column of a query into a []string. Used for the
// pre-delete flow-ID capture inside DeleteUser's transaction.
func rowsOfStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
