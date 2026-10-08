package database

import (
	"context"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
)

// The account domain: the user row and what hangs off it (credentials, sessions, tokens, usage, the audit trail).
func init() {
	registerLifecycle(lifecycleDomain{
		name: "accounts",
		tables: []tableClaim{
			{table: "users", export: "user", erase: erasedByStep, org: orgNotKeyed,
				noPurge: "accounts are erased on request, never aged out"},
			{table: "refresh_tokens", export: "sessions", erase: erasedByStep, org: orgNotKeyed},
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
			{table: "usage_metrics", noExport: "not in the bundle yet (open: AI usage records)",
				erase: erasedByStep,
				org:   orgKept, orgWhy: "spend history attributed to the organisation; nothing reads a deleted organisation's rows and the retention window ages them out"},
			{table: "audit_events", export: "auditEvents", erase: erasedByStep,
				org: orgKept, orgWhy: "the audit trail keeps the organisation each action happened in"},
		},
		export: []exportSection{
			section("user", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) (*interfaces.User, error) {
				return b.LoadUserByID(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v *interfaces.User) { out.User = v }),
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
			eraseByUser(erasePersonal, "usage_metrics", `DELETE FROM usage_metrics WHERE user_id = $1`),
			eraseByUser(erasePersonal, "provider_keys", `DELETE FROM provider_keys WHERE user_id = $1`),
			// api_tokens, user_tokens, identity_links, idempotency_keys, user_settings, flow_collaborators and the
			// personal Azure registry cascade with the row.
			eraseByUser(eraseAccount, "users", `DELETE FROM users WHERE id = $1`),
		},
		purge: []purgeStep{
			{table: "refresh_tokens", cond: `expires_at < NOW()`,
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
