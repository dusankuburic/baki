package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The account domain: the user row and what hangs off it (credentials, sessions, tokens, usage, the audit trail).
func init() {
	registerLifecycle(lifecycleDomain{
		name: "accounts",
		tables: []tableClaim{
			{table: "users", export: "user", erase: erasedByStep, org: orgNotKeyed,
				noPurge: "accounts are erased on request, never aged out"},
			{table: "user_avatars", export: "avatar", erase: erasedByCascade, org: orgNotKeyed,
				noPurge: "an avatar lives until its owner removes it"},
			// The step deletes them; since v48 the users row's foreign key does too (the step stays for a database
			// rolled back below v48).
			{table: "refresh_tokens", export: "sessions", erase: erasedByStep | erasedByCascade, org: orgNotKeyed},
			{table: "token_blacklist", noExport: "revoked token ids, no user identity",
				erase: notPersonal, eraseWhy: "a token id (jti) and its expiry", org: orgNotKeyed},
			{table: "identity_links", noExport: "not in the bundle yet (open: the SSO links are personal data)",
				erase: erasedByCascade, org: orgNotKeyed, noPurge: "a link lives as long as its account"},
			{table: "api_tokens", export: "apiTokens", erase: erasedByCascade, org: orgNotKeyed},
			{table: "user_tokens", noExport: "single-use verification and password-reset token hashes",
				erase: erasedByCascade, org: orgNotKeyed},
			{table: "idempotency_keys", noExport: "stored responses of retried requests, kept for 24 hours",
				erase: erasedByCascade, org: orgNotKeyed},
			{table: "provider_keys", noExport: "encrypted credentials: secrets never leave the keystore (the Azure resources' public configuration is in azureRegistry)",
				erase: erasedByStep, org: orgNotKeyed, noPurge: "a credential lives until its owner removes it"},
			{table: "usage_metrics", export: "aiUsage", erase: erasedByStep,
				org: orgKept, orgWhy: "spend history attributed to the organisation; nothing reads a deleted organisation's rows and the retention window ages them out"},
			{table: "audit_events", export: "auditEvents", erase: erasedByStep,
				org: orgKept, orgWhy: "the audit trail keeps the organisation each action happened in"},
		},
		export: []exportSection{
			section("user", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) (*interfaces.User, error) {
				return b.LoadUserByID(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v *interfaces.User) { out.User = v }),
			// The uploaded image is personal data; absent when the user never uploaded one (the section reads as null).
			section("avatar", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) (*interfaces.UserAvatar, error) {
				a, err := b.LoadUserAvatar(ctx, x.userID)
				if errors.Is(err, interfaces.ErrNotFound) {
					return nil, nil
				}
				return a, err
			}, func(out *interfaces.UserDataExport, v *interfaces.UserAvatar) { out.Avatar = v }),
			section("auditEvents", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]interfaces.ExportedAuditEvent, error) {
				events, err := b.exportAuditEvents(ctx, x.userID)
				return interfaces.NewExportedAuditEvents(events), err
			}, func(out *interfaces.UserDataExport, v []interfaces.ExportedAuditEvent) { out.AuditEvents = v }),
			section("apiTokens", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.APIToken, error) {
				return b.ListAPITokens(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v []*interfaces.APIToken) { out.APITokens = v }),
			// Empty serialises as [] rather than null, so a consumer can tell "none" from "section missing".
			section("sessions", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]interfaces.RefreshTokenInfo, error) {
				sessions, err := b.ListUserRefreshTokens(ctx, x.userID)
				if sessions == nil {
					sessions = []interfaces.RefreshTokenInfo{}
				}
				return sessions, err
			}, func(out *interfaces.UserDataExport, v []interfaces.RefreshTokenInfo) { out.Sessions = v }),
			// The user's AI usage, personal and organisation-billed (R-security-11). An erasure takes the user's id
			// off the organisation-billed rows, so they leave the export with the account, as they leave the user.
			section("aiUsage", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]interfaces.UsageMetric, error) {
				return b.exportAIUsage(ctx, x.userID, aiUsageExportPageSize)
			}, func(out *interfaces.UserDataExport, v []interfaces.UsageMetric) { out.AIUsage = v }),
		},
		erase: []eraseStep{
			// The forensic trail stays; the user's personal data on it goes: the email, IP, token and user agent of
			// the user's own actions (with the IP they identify the person as well as the user id did), and the user
			// agent of the pre-auth rows (login, register, password reset: no user_id, the user as their resource),
			// whose other columns stay as they always have.
			eraseByUser(eraseIdentity, "audit_events", `UPDATE audit_events SET
		email      = CASE WHEN user_id = $1 THEN '' ELSE email END,
		ip         = CASE WHEN user_id = $1 THEN '' ELSE ip END,
		token_id   = CASE WHEN user_id = $1 THEN NULL ELSE token_id END,
		user_agent = NULL,
		user_id    = CASE WHEN user_id = $1 THEN '' ELSE user_id END
		WHERE user_id = $1 OR (resource_type = 'user' AND resource_id = $1)`),
			// A failed login for an address no account had yet names the user by email: the address and the user
			// agent go, the attempt and its IP stay.
			eraseByEmail(eraseIdentity, "audit_events", `UPDATE audit_events SET resource_id = '', user_agent = NULL
		WHERE resource_type = 'email' AND lower(resource_id) = lower($1)`),
			eraseByUser(erasePersonal, "refresh_tokens", `DELETE FROM refresh_tokens WHERE user_id = $1`),
			// Spend billed to an organisation is the organisation's: deleting it would lower its daily budget sum
			// and its cost history. It stays without the user (the identity phase runs first); the rest is personal.
			eraseByUser(eraseIdentity, "usage_metrics", `UPDATE usage_metrics SET user_id = '' WHERE user_id = $1 AND org_id <> ''`),
			eraseByUser(erasePersonal, "usage_metrics", `DELETE FROM usage_metrics WHERE user_id = $1`),
			eraseByUser(erasePersonal, "provider_keys", `DELETE FROM provider_keys WHERE user_id = $1`),
			// api_tokens, user_tokens, identity_links, idempotency_keys, user_settings, flow_collaborators and the
			// personal Azure registry cascade with the row.
			eraseByUser(eraseAccount, "users", `DELETE FROM users WHERE id = $1`),
		},
		purge: []purgeStep{
			// A row per sign-in and per refresh (rotation), so batched: a backlog in one DELETE ran into the
			// statement timeout and never shrank.
			{table: "refresh_tokens", cond: `expires_at < NOW()`, batched: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.RefreshTokens }},
			{table: "api_tokens", cond: `expires_at IS NOT NULL AND expires_at < NOW()`,
				count: func(r *interfaces.PurgeResult) *int { return &r.APITokens }},
			{table: "user_tokens", cond: `expires_at < NOW()`,
				count: func(r *interfaces.PurgeResult) *int { return &r.UserTokens }},
			// Expired blacklist entries are pure dead weight, so purge them unconditionally — previously only a
			// per-process goroutine did this, and it could die or be absent on some replicas, growing the table unbounded.
			{table: "token_blacklist", cond: `expires_at < NOW()`, batched: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.TokenBlacklist }},
			// An Idempotency-Key answers retries for interfaces.IdempotencyKeyRetention; after that its stored
			// response (sealed, possibly a credential) is dead weight too.
			{table: "idempotency_keys", cond: `expires_at < NOW()`, batched: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.IdempotencyKeys }},
			{table: "audit_events", cond: `created_at < NOW() - ($1 * INTERVAL '1 day')`, batched: true, windowed: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.AuditEvents }},
			// usage_metrics is append-only and drives the dashboard's cost/trend views; the audit window.
			{table: "usage_metrics", cond: `created_at < NOW() - ($1 * INTERVAL '1 day')`, batched: true, windowed: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.UsageMetrics }},
		},
	})
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
			return nil, fmt.Errorf("list audit: %w", err)
		}
		audit = append(audit, page...)
		if len(page) < auditPageSize {
			return audit, nil
		}
		last := page[len(page)-1]
		after = &auditCursor{createdAt: last.CreatedAt, id: last.ID}
	}
}

// aiUsageExportPageSize is the rows one page of the AI usage walk reads.
const aiUsageExportPageSize = 1000

// exportAIUsageSQL reads one page of the user's usage rows after the keyset (created_at, id), oldest first. The
// created_at bound is spelled out beside the row comparison so the walk range-scans usage_metrics_user_created_idx
// (user_id, created_at); id only orders the rows that share a timestamp.
const exportAIUsageSQL = `
	SELECT id, user_id, org_id, provider, model, served_model, prompt_tokens, completion_tokens, created_at
	  FROM usage_metrics
	 WHERE user_id = $1 AND created_at >= $2 AND (created_at, id) > ($2, $3)
	 ORDER BY created_at, id
	 LIMIT $4`

// exportAIUsage walks the user's AI usage rows, personal and organisation-billed, as a keyset over
// (created_at, id), the pattern of exportAuditEvents: a row recorded mid-walk lands after the cursor or before it,
// never shifting a page boundary. [] when there are none.
func (b *PostgresStorageBackend) exportAIUsage(ctx context.Context, userID string, pageSize int) ([]interfaces.UsageMetric, error) {
	out := []interfaces.UsageMetric{}
	afterAt, afterID := time.Unix(0, 0).UTC(), ""
	for {
		page, err := b.aiUsagePage(ctx, userID, afterAt, afterID, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
		last := page[len(page)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
}

// aiUsagePage is one page of exportAIUsage's walk: the user's rows after (afterAt, afterID).
func (b *PostgresStorageBackend) aiUsagePage(ctx context.Context, userID string, afterAt time.Time, afterID string, pageSize int) ([]interfaces.UsageMetric, error) {
	rows, err := b.query(ctx).QueryContext(ctx, exportAIUsageSQL, userID, afterAt, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("list ai usage: %w", err)
	}
	defer rows.Close()
	var page []interfaces.UsageMetric
	for rows.Next() {
		var m interfaces.UsageMetric
		if err := rows.Scan(&m.ID, &m.UserID, &m.OrgID, &m.Provider, &m.Model, &m.ServedModel,
			&m.PromptTokens, &m.CompletionTokens, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ai usage: %w", err)
		}
		m.CreatedAt = m.CreatedAt.UTC()
		page = append(page, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ai usage: %w", err)
	}
	return page, nil
}
