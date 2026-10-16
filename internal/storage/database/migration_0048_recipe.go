package database

// Recipe and preflight SQL for migration 0048 integrity constraints.
// These queries are shared between bakicli migrate preflight, the verification
// tests, and operator documentation (docs/deployment.md, operations.md).
const (
	// DuplicateEmailGroupsSQL lists email addresses that collide up to case and
	// the comma-separated user IDs in each group, oldest first.
	DuplicateEmailGroupsSQL = `SELECT lower(email) AS address, string_agg(id, ', ' ORDER BY created_at, id) AS ids
FROM users GROUP BY lower(email) HAVING count(*) > 1 ORDER BY 1`

	// DuplicateEmailCompareSQL inspects the accounts within a single duplicate group
	// to help the operator decide which account to keep.
	DuplicateEmailCompareSQL = `SELECT u.id, u.email, u.email_verified, u.password <> '' AS has_password, u.created_at,
       (SELECT max(t.created_at) FROM refresh_tokens t WHERE t.user_id = u.id) AS last_sign_in,
       (SELECT count(*) FROM identity_links l WHERE l.user_id = u.id) AS sso_links,
       (SELECT count(*) FROM api_tokens a WHERE a.user_id = u.id) AS api_tokens,
       (SELECT count(*) FROM flows f WHERE f.owner_id = u.id) AS flows,
       (SELECT count(*) FROM org_members m WHERE m.user_id = u.id) AS organisations
FROM users u WHERE lower(u.email) = '<address>' ORDER BY u.email_verified DESC, u.created_at, u.id`

	// DuplicateEmailResolveSQL is the single transaction that resolves a case-duplicate email group:
	// it renames the non-kept accounts to unroutable addresses, revokes sessions/tokens, transfers
	// SSO links if verified, and lower-cases the kept account's email.
	DuplicateEmailResolveSQL = `BEGIN;
UPDATE users SET email = 'duplicate+' || id || '@example.invalid', password = '', updated_at = NOW() WHERE id IN (<others>);
UPDATE refresh_tokens SET revoked = TRUE WHERE user_id IN (<others>) AND NOT revoked;
DELETE FROM api_tokens WHERE user_id IN (<others>);
DELETE FROM user_tokens WHERE user_id IN (<others>);
UPDATE identity_links SET user_id = '<kept>' WHERE user_id IN (SELECT id FROM users WHERE id IN (<others>) AND email_verified) AND (SELECT email_verified FROM users WHERE id = '<kept>');
DELETE FROM identity_links WHERE user_id IN (<others>);
UPDATE users SET email = lower(email), updated_at = NOW() WHERE id = '<kept>';
COMMIT;`

	// DuplicateInviteTokenGroupsSQL lists groups of organisation invites that share a token_hash.
	DuplicateInviteTokenGroupsSQL = `SELECT string_agg(id, ', ' ORDER BY created_at, id) AS ids
FROM org_invites GROUP BY token_hash HAVING count(*) > 1`
)
