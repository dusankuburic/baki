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

	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage/interfaces"
)

// DeleteUser performs GDPR-style account erasure for userID: removes owned flows (cascading versions/
// analysis/triage/baselines/conversations) and all per-user rows (tokens, settings, usage, org memberships,
// invites for their email), and anonymizes PII authored on shared rows kept for forensic integrity (audit_events
// email/IP, flow_versions/finding_status/flow_baselines created_by/updated_by). Runs in its own transaction WITHOUT
// app.current_user_id — correct for a maintenance path that must touch rows across all of the user's flows while the
// Go layer already authenticated the caller. Idempotent: a missing user is not an error.
func (b *PostgresStorageBackend) DeleteUser(ctx context.Context, userID string) error {
	var ownedFlowIDs []string
	if err := b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		var err error
		ownedFlowIDs, err = b.deleteUserInTx(ctx, tx, userID)
		return err
	}); err != nil {
		return err
	}

	// Best-effort blob cleanup for the flows just removed, deferred via txhook.Register: when this
	// request carries the middleware's RLS transaction, withTx JOINS it and nothing is durable until the
	// middleware commits — deleting blobs here would orphan content a rollback puts back.
	if b.blobClient != nil {
		for _, fid := range ownedFlowIDs {
			flowID := fid
			txhook.Register(ctx, func(context.Context) {
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
		// A transient scan error must propagate (a gap in an audited GDPR flow), but "no rows" is the
		// documented idempotent case: the email is unknown and the remaining sweeps clean up any residue.
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
	// forensic integrity; only personal data is stripped (the user agent and the token too: with the IP they
	// identify the person as well as the user id did). The user's pre-auth rows (login, register, password
	// reset: no user_id, the user as their resource) lose the user agent they carry since v33; their other
	// columns stay as they always have. One statement, so erasure keeps its statement count.
	if _, err = tx.ExecContext(ctx, `UPDATE audit_events SET
		email      = CASE WHEN user_id = $1 THEN '' ELSE email END,
		ip         = CASE WHEN user_id = $1 THEN '' ELSE ip END,
		token_id   = CASE WHEN user_id = $1 THEN NULL ELSE token_id END,
		user_agent = NULL,
		user_id    = CASE WHEN user_id = $1 THEN '' ELSE user_id END
		WHERE user_id = $1 OR (resource_type = 'user' AND resource_id = $1)`, userID); err != nil {
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

// ExportUserData assembles the user's personal data for a data-subject access/portability request:
// profile, owned flows, settings, API tokens and audit history, on the caller's RLS-scoped context.
func (b *PostgresStorageBackend) ExportUserData(ctx context.Context, userID string) (*interfaces.UserDataExport, error) {
	user, err := b.LoadUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Page through ALL of the user's flows: ListFlows clamps Limit<=0 to a default page size, so a single
	// call would silently truncate the export — unacceptable for a data-subject access request. The walk is a
	// keyset (FlowFilter.AfterID): every page costs the same instead of OFFSET re-reading earlier pages.
	// FlowSortIDAsc, not the default: updated_at is MUTABLE — a flow saved mid-export jumps to the front and
	// a cursor on a mutable key would chase that jump or skip the rows it displaced; the immutable primary
	// key means an update cannot move a row across the cursor.
	const exportPageSize = 500 // ListFlows' maximum page size
	var flows []*interfaces.FlowDocument
	filter := interfaces.FlowFilter{UserID: userID, SortBy: interfaces.FlowSortIDAsc, Limit: exportPageSize}
	for {
		page, err := b.ListFlows(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("export: list flows: %w", err)
		}
		flows = append(flows, page...)
		if len(page) < exportPageSize {
			break
		}
		filter.AfterID = page[len(page)-1].ID
	}
	// A data-subject export must be complete or fail: this catches a flow whose content blob is
	// permanently missing (ListFlows returns it with nil content) instead of silently shipping without it.
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
		AzureRegistry:   sections.azure,
		ExportedAt:      time.Now().UTC(),
	}, nil
}

// exportAuditEvents pages through the user's whole audit history (a single Limit:1000 call silently
// truncated the trail for active accounts). The page size IS ListAuditEvents' clamp: it was 1000 while the
// clamp was 500, so every page came back "short" and the walk stopped after the first one — at most 500
// events, no error, no truncation marker. The walk is a keyset (see listAuditEventsSQL): OFFSET re-read every
// earlier page per page, and on an append-heavy newest-first table an event logged mid-walk shifted the page
// boundary and repeated a row; a keyset cannot.
func (b *PostgresStorageBackend) exportAuditEvents(ctx context.Context, userID string) ([]*interfaces.AuditEvent, error) {
	const auditPageSize = maxAuditLimit
	audit := []*interfaces.AuditEvent{}
	var after *auditCursor
	for {
		page, err := b.listAuditEvents(ctx, interfaces.AuditFilter{UserID: userID, Limit: auditPageSize}, after)
		if err != nil {
			return nil, fmt.Errorf("export: list audit: %w", err)
		}
		audit = append(audit, page...)
		if len(page) < auditPageSize {
			return audit, nil
		}
		last := page[len(page)-1]
		after = &auditCursor{createdAt: last.CreatedAt, id: last.ID}
	}
}

// userExportSections are the bundle sections that follow the flows: chat history on the user's own flows,
// the comments and triage decisions they authored, their organisations, active sessions and personal Azure registry.
type userExportSections struct {
	conversations []*interfaces.ExportedConversation
	comments      []*interfaces.FindingComment
	triage        []*interfaces.FindingStatus
	orgs          []*interfaces.Organisation
	sessions      []interfaces.RefreshTokenInfo
	azure         *interfaces.ExportedAzureRegistry
}

// exportUserSections loads userExportSections for both export paths, so the buffered and the streamed bundle
// cannot drift apart again (the sections were once added to ExportUserData only). flowIDs are the user's
// owned flows, which scope conversations and triage.
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
	if out.azure, err = b.exportAzureRegistry(ctx, userID); err != nil {
		return nil, fmt.Errorf("export: azure registry: %w", err)
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

// exportConversations returns every stored chat conversation on the given (owned) flows — chat about your
// own flow is user-authored content and belongs in the bundle.
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

// exportTriageDecisions returns the triage rows the user last updated on the given (owned) flows — rows on
// those flows last touched by a collaborator are that collaborator's content and stay out.
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

// exportFlowPageSize bounds how many flows (with content) ExportUserDataTo holds in memory at once — smaller
// than ListFlows' max so peak memory stays one modest page rather than every flow's content.
const exportFlowPageSize = 100

// ExportUserDataTo streams the same JSON bundle as ExportUserData to w, but fetches flows one page at a time,
// so peak memory is bounded to a single page's content. Callers MUST treat w as tainted if a non-nil error is
// returned (partial JSON may have been written) — the HTTP handler streams to a temp file and only forwards it
// on success, preserving the "complete or fail" guarantee a data-subject export requires.
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
	// Keyset walk (AfterID) over the immutable id ordering, for the same reasons as ExportUserData
	// above: a concurrent save must not be able to drop a flow, and each page must cost the same.
	filter := interfaces.FlowFilter{UserID: userID, SortBy: interfaces.FlowSortIDAsc, Limit: exportFlowPageSize}
	for {
		page, err := b.ListFlows(ctx, filter)
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
		filter.AfterID = page[len(page)-1].ID
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
		{`,"azureRegistry":`, sections.azure},
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

// purgeBatchSize is how many rows one DELETE of a growth table removes
// (purgeBatched). A variable so tests can shrink it.
var purgeBatchSize = 5000

// purgeBatchedSQL renders the batched form of `DELETE FROM table WHERE cond`: rows are picked by physical
// address (ctid) through the condition's own index, LIMIT $batch at a time, so each statement finishes well
// inside statement_timeout whatever the backlog.
func purgeBatchedSQL(table, cond string, batchParam int) string {
	return fmt.Sprintf(`DELETE FROM %s WHERE ctid = ANY(ARRAY(SELECT ctid FROM %s WHERE %s LIMIT $%d))`, table, table, cond, batchParam)
}

// purgeBatched deletes every row of table matching cond, purgeBatchSize rows per statement, until a statement
// removes nothing. Returns the rows removed so far with the first error, so a partial sweep is still counted.
func (b *PostgresStorageBackend) purgeBatched(ctx context.Context, table, cond string, args ...any) (int, error) {
	stmt := purgeBatchedSQL(table, cond, len(args)+1)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := b.db.ExecContext(ctx, stmt, append(append([]any{}, args...), purgeBatchSize)...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n == 0 {
			return total, nil
		}
	}
}

// PurgeExpiredData removes stale rows whose retention has elapsed — intended for a periodic background job
// (single-replica). auditRetentionDays <= 0 keeps audit_events indefinitely (only expired tokens/invites purge).
// The tables that grow without bound (audit_events, flow_analysis_history, usage_metrics, gov_alerts,
// token_blacklist, idempotency_keys) are purged in batches of purgeBatchSize rows per statement: a single unbatched DELETE over a
// large backlog timed out on every tick and never shrank it. Each table is swept independently — an error is
// recorded and the remaining tables still run; the result carries every table's count, the error joins all failures.
func (b *PostgresStorageBackend) PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*interfaces.PurgeResult, error) {
	out := &interfaces.PurgeResult{}
	var errs []error

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
		// An expired share link can never be redeemed again, so its row is dead weight — and one
		// that still names a flow. No-expiry rows are permanent links; kept.
		{`DELETE FROM share_tokens WHERE expires_at IS NOT NULL AND expires_at < NOW()`, &out.ShareTokens},
	}
	// Deliberately NOT purged by age: conversations, knowledge documents and finding comments are user
	// content, not telemetry — they go when their flow/org goes (ON DELETE CASCADE), never on a timer.
	for _, s := range stmts {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("purge: %w", err))
			return out, errors.Join(errs...)
		}
		res, err := b.db.ExecContext(ctx, s.sql)
		if err != nil {
			errs = append(errs, fmt.Errorf("purge: %w", err))
			continue
		}
		n, _ := res.RowsAffected()
		*s.dest = int(n)
	}

	type batched struct {
		table, cond string
		args        []any
		dest        *int
	}
	growth := []batched{
		// Expired blacklist entries are pure dead weight, so purge them unconditionally — previously only a
		// per-process goroutine did this, and it could die or be absent on some replicas, growing the table unbounded.
		{"token_blacklist", `expires_at < NOW()`, nil, &out.TokenBlacklist},
		// An Idempotency-Key answers retries for interfaces.IdempotencyKeyRetention; after that its stored
		// response (sealed, possibly a credential) is dead weight too.
		{"idempotency_keys", `expires_at < NOW()`, nil, &out.IdempotencyKeys},
	}
	if auditRetentionDays > 0 {
		window := []any{auditRetentionDays}
		growth = append(growth,
			batched{"audit_events", `created_at < NOW() - ($1 * INTERVAL '1 day')`, window, &out.AuditEvents},
			// flow_analysis_history is append-only and the dashboard only
			// reads the last 14–30 days; age it out with the audit window.
			batched{"flow_analysis_history", `analyzed_at < NOW() - ($1 * INTERVAL '1 day')`, window, &out.FlowAnalysisHistory},
			// usage_metrics is append-only and drives the same dashboard's
			// cost/trend views; same retention window.
			batched{"usage_metrics", `created_at < NOW() - ($1 * INTERVAL '1 day')`, window, &out.UsageMetrics},
			// gov_alerts is one row per alert per sweep, forever — ageing rows
			// out cannot cause a re-alert (the scanner's dedup is in-memory).
			batched{"gov_alerts", `created_at < NOW() - ($1 * INTERVAL '1 day')`, window, &out.GovAlerts},
		)
	}
	for _, g := range growth {
		n, err := b.purgeBatched(ctx, g.table, g.cond, g.args...)
		*g.dest = n
		if err != nil {
			errs = append(errs, fmt.Errorf("purge %s: %w", g.table, err))
			if ctx.Err() != nil {
				break // nothing further can run
			}
		}
	}
	return out, errors.Join(errs...)
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
