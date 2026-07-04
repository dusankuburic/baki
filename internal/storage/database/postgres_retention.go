package database

import (
	"context"
	"database/sql"
	"fmt"
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
func (b *PostgresStorageBackend) DeleteUser(ctx context.Context, userID string) (err error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete user: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Capture email + owned flow IDs before deleting: org_invites is keyed by
	// email (not user_id), and owned flows' blob content must be cleaned up.
	var email string
	_ = tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)

	ownedFlowIDs, _ := rowsOfStrings(ctx, tx, `SELECT id FROM flows WHERE owner_id = $1`, userID)

	// Owned flows: ON DELETE CASCADE removes their versions, analysis history,
	// triage, baselines, conversations and collaborator rows.
	if _, err = tx.ExecContext(ctx, `DELETE FROM flows WHERE owner_id = $1`, userID); err != nil {
		return fmt.Errorf("delete user flows: %w", err)
	}

	// Anonymize PII on surviving rows. The events/versions are kept for
	// forensic integrity; only personal data is stripped.
	if _, err = tx.ExecContext(ctx, `UPDATE audit_events SET email = '', ip = '', user_id = '' WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("anonymize audit_events: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flow_versions SET created_by = '' WHERE created_by = $1`, userID); err != nil {
		return fmt.Errorf("anonymize flow_versions: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE finding_status SET updated_by = '' WHERE updated_by = $1`, userID); err != nil {
		return fmt.Errorf("anonymize finding_status: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flow_baselines SET created_by = '' WHERE created_by = $1`, userID); err != nil {
		return fmt.Errorf("anonymize flow_baselines: %w", err)
	}

	// Per-user rows that have no ON DELETE CASCADE foreign key.
	for _, stmt := range []string{
		`DELETE FROM refresh_tokens WHERE user_id = $1`,
		`DELETE FROM usage_metrics WHERE user_id = $1`,
		`DELETE FROM provider_keys WHERE user_id = $1`,
		`DELETE FROM org_members WHERE user_id = $1`,
	} {
		if _, err = tx.ExecContext(ctx, stmt, userID); err != nil {
			return fmt.Errorf("delete per-user rows: %w", err)
		}
	}
	if email != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM org_invites WHERE email = $1`, email); err != nil {
			return fmt.Errorf("delete org_invites: %w", err)
		}
	}

	// The user row last; api_tokens, user_settings, identity_links and
	// flow_collaborators cascade via their ON DELETE CASCADE foreign keys.
	if _, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("delete user: commit: %w", err)
	}

	// Best-effort blob cleanup for the flows that were just removed. Runs only
	// after the DB commit so a rolled-back erasure never orphans surviving
	// content. Detached + bounded so a slow/hung blob store can't block
	// request teardown or leak.
	if b.blobClient != nil {
		for _, fid := range ownedFlowIDs {
			go b.deleteFlowBlobsDetached(fid)
		}
	}
	return nil
}

// deleteFlowBlobsDetached removes a flow's blob prefix with a bounded context
// and panic recovery (project convention for background goroutines).
func (b *PostgresStorageBackend) deleteFlowBlobsDetached(flowID string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("DeleteUser blob cleanup goroutine panicked", "flow_id", flowID, "err", r)
		}
	}()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.deleteBlobs(cctx, fmt.Sprintf("flows/%s/", flowID)); err != nil {
		logger.Warn("DeleteUser: failed to delete flow blobs", "flow_id", flowID, "error", err)
	}
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
	flows, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: userID, Limit: 0})
	if err != nil {
		return nil, fmt.Errorf("export: list flows: %w", err)
	}
	settings, _ := b.LoadUserSettings(ctx, userID)
	tokens, err := b.ListAPITokens(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: list tokens: %w", err)
	}
	audit, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: userID, Limit: 1000})
	if err != nil {
		return nil, fmt.Errorf("export: list audit: %w", err)
	}
	return &interfaces.UserDataExport{
		User:        user,
		Flows:       flows,
		Settings:    settings,
		AuditEvents: audit,
		APITokens:   tokens,
		ExportedAt:  time.Now().UTC(),
	}, nil
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
	}
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
