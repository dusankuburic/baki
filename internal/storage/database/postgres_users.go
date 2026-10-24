package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// ---- User operations ----
// Reads go through b.query(ctx): inside an authenticated request that is the request's RLS transaction —
// users, refresh_tokens, audit_events and usage_metrics carry no RLS policy, but reading via b.db would take
// a SECOND pooled connection (the pool-exhaustion deadlock, rls.go) and see none of the request's own
// uncommitted writes. Writes stay on b.db deliberately: login counters/lockouts, token rotation, audit events
// and usage rows must persist even when the middleware rolls back on >= 400, and role/password/profile
// updates are single autocommits whose visibility would change if they joined the request transaction.

func (b *PostgresStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	return saveUserOn(ctx, b.db, user)
}

// saveUserOn is SaveUser's upsert on exec: the pool, or the transaction of SaveUserWithIdentityLink.
func saveUserOn(ctx context.Context, exec DBTX, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now

	_, err := exec.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			email                 = EXCLUDED.email,
			password              = EXCLUDED.password,
			role                  = EXCLUDED.role,
			email_verified        = EXCLUDED.email_verified,
			failed_login_attempts = EXCLUDED.failed_login_attempts,
			locked_until          = EXCLUDED.locked_until,
			display_name          = EXCLUDED.display_name,
			avatar_url            = EXCLUDED.avatar_url,
			updated_at            = EXCLUDED.updated_at`,
		user.ID, user.Email, user.Password, string(user.Role), user.EmailVerified, user.FailedLoginAttempts, user.LockedUntil, user.DisplayName, user.AvatarURL, user.CreatedAt, user.UpdatedAt,
	)
	if err != nil {
		if isPgErrCode(err, pgErrUniqueViolation) {
			return interfaces.ErrEmailExists
		}
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// bootstrapLockKey serializes the registrations that may make the first user the admin.
const bootstrapLockKey int64 = 0x62616b69626f6f74 // "bakiboot"

// CreateUser atomically inserts a new user; on an empty users table a registration (auth.WithAllowBootstrap)
// becomes the RoleAdmin. ErrEmailExists on a unique violation.
func (b *PostgresStorageBackend) CreateUser(ctx context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now

	var role auth.Role
	var emailVerified bool
	err := b.withOwnTx(ctx, sql.LevelDefault, func(_ context.Context, tx *sql.Tx) error {
		// EXISTS, not COUNT(*): the question is only "is this the first user".
		anyUser := func() (bool, error) {
			var any bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&any); err != nil {
				return false, fmt.Errorf("check for existing users: %w", err)
			}
			return any, nil
		}
		exists, err := anyUser()
		if err != nil {
			return err
		}
		// Only a registration on an apparently empty table takes the bootstrap lock and asks again: of two
		// concurrent first registrations the second sees the first's committed row. Every other registration runs at
		// READ COMMITTED with no lock: under SERIALIZABLE the emptiness read made a burst of sign-ups fail.
		if !exists && auth.AllowBootstrap(ctx) {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockKey); err != nil {
				return fmt.Errorf("bootstrap lock: %w", err)
			}
			if exists, err = anyUser(); err != nil {
				return err
			}
		}
		// Bootstrap: the very first user becomes admin — but ONLY with auth.WithAllowBootstrap (the
		// registration path); SSO JIT provisioning must not claim admin on a fresh deployment.
		role = user.Role
		emailVerified = user.EmailVerified
		if !exists && auth.AllowBootstrap(ctx) {
			role = auth.RoleAdmin
			// The bootstrap admin is email-verified: nobody to impersonate on an empty deployment,
			// and a no-SMTP deployment would otherwise lock out its own first administrator.
			emailVerified = true
		}

		if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			user.ID, user.Email, user.Password, string(role), emailVerified, user.FailedLoginAttempts, user.LockedUntil, user.DisplayName, user.AvatarURL, user.CreatedAt, user.UpdatedAt,
		); err != nil {
			if isPgErrCode(err, pgErrUniqueViolation) {
				return interfaces.ErrEmailExists
			}
			return fmt.Errorf("insert user: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	user.Role = role
	user.EmailVerified = emailVerified
	return nil
}

func (b *PostgresStorageBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) {
	email = strings.ToLower(email)
	var user interfaces.User
	var roleStr string
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.EmailVerified, &user.FailedLoginAttempts, &user.LockedUntil, &user.DisplayName, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by email: %w", err)
	}
	user.Role = auth.Role(roleStr)
	return &user, nil
}

func (b *PostgresStorageBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) {
	var user interfaces.User
	var roleStr string
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at FROM users WHERE id = $1`,
		id,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.EmailVerified, &user.FailedLoginAttempts, &user.LockedUntil, &user.DisplayName, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by id: %w", err)
	}
	user.Role = auth.Role(roleStr)
	return &user, nil
}

// LoadUsersByIDs resolves multiple users in one query (avoids N+1); duplicate IDs are de-duplicated.
func (b *PostgresStorageBackend) LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*interfaces.User, error) {
	out := make(map[string]*interfaces.User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// De-duplicate and query with `= ANY($1)` — no hand-built placeholder list and its
	// latent >65535-parameter cap.
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, email, role, email_verified, failed_login_attempts, locked_until, created_at, updated_at FROM users WHERE id = ANY($1)`,
		unique)
	if err != nil {
		return nil, fmt.Errorf("load users by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u interfaces.User
		var roleStr string
		if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Role = auth.Role(roleStr)
		uu := u
		out[u.ID] = &uu
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := b.query(ctx).QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (b *PostgresStorageBackend) ListUsers(ctx context.Context, limit, offset int) ([]*interfaces.User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// `id` as a final tiebreaker: this is offset-paginated, created_at ties in the same microsecond, and
	// SQL orders tied rows arbitrarily — without it the walk could drop or repeat users across a page boundary.
	rows, err := b.query(ctx).QueryContext(ctx, `SELECT `+listedUserColumns+` FROM users ORDER BY created_at DESC, id ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []*interfaces.User
	for rows.Next() {
		u, err := scanListedUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (b *PostgresStorageBackend) ListAdmins(ctx context.Context) ([]*interfaces.User, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `SELECT `+listedUserColumns+` FROM users WHERE role = 'admin'`)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()

	var users []*interfaces.User
	for rows.Next() {
		u, err := scanListedUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// listedUserColumns is the projection every list-style user read shares (display name and avatar included —
// the admin table shows them); scanListedUser reads one row of it.
const listedUserColumns = `id, email, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at`

func scanListedUser(rows *sql.Rows) (*interfaces.User, error) {
	var u interfaces.User
	var roleStr string
	if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil, &u.DisplayName, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.Role = auth.Role(roleStr)
	return &u, nil
}

// validRolesSQL is the role set QueryUsers treats as assigned; any other
// stored value counts as interfaces.UserRoleNone.
const validRolesSQL = `('admin', 'member', 'viewer', 'guest')`

// queryUsersSQL builds QueryUsers' two statements (exposed for the plan tests): the count of the users matching q's
// filters, and q's page of them.
func queryUsersSQL(q interfaces.UserQuery) (count string, countArgs []any, page string, pageArgs []any) {
	// A cap clamps DOWN to the maximum (see ListAuditEvents).
	const (
		defaultUserLimit = 100
		maxUserLimit     = 500
	)
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultUserLimit
	case limit > maxUserLimit:
		limit = maxUserLimit
	}
	offset := max(q.Offset, 0)

	args := sqlArgs{}
	where := []string{}
	if needle := strings.ToLower(strings.TrimSpace(q.Q)); needle != "" {
		// LIKE on the indexed expressions, the needle escaped: users_email_trgm_idx and users_display_name_trgm_idx
		// (v49) serve it as a BitmapOr (strpos, which needed no escaping, could only be checked row by row). A
		// trigram index draws nothing from a substring without a run of three letters or digits, which would read
		// every account: such a needle matches a prefix (the index draws the start-of-word trigrams from its letters).
		pattern := "%" + likeLiteral(needle) + "%"
		if q.Prefix || interfaces.ShortSearchTerm(needle) {
			pattern = likeLiteral(needle) + "%"
		}
		p := args.add(pattern)
		where = append(where, "(lower(email) LIKE "+p+" OR lower(display_name) LIKE "+p+")")
	}
	switch q.Role {
	case "":
	case interfaces.UserRoleNone:
		where = append(where, "role NOT IN "+validRolesSQL)
	default:
		where = append(where, "role = "+args.add(q.Role))
	}
	clause := func(where []string) string {
		if len(where) == 0 {
			return ""
		}
		return " WHERE " + strings.Join(where, " AND ")
	}

	pa := append(sqlArgs{}, args...)
	pageWhere := where
	if a := q.After; a != nil {
		// The users after the cursor in the list's order, served by users_created_id_idx (created_at DESC, id); the
		// count stays the filters' whole match.
		at, id := pa.add(a.At), pa.add(a.ID)
		pageWhere = append(append([]string{}, where...), "created_at <= "+at+" AND (created_at < "+at+" OR id > "+id+")")
		offset = 0
	}
	order := " ORDER BY created_at DESC, id ASC LIMIT " + pa.add(limit) + " OFFSET " + pa.add(offset)
	// #nosec G202 -- only generated "$N" placeholders and constants are concatenated; all values are parameterized args.
	return `SELECT COUNT(*) FROM users` + clause(where), args, `SELECT ` + listedUserColumns + ` FROM users` + clause(pageWhere) + order, pa
}

// QueryUsers backs the admin user list: one filtered, ordered page plus the
// number of matching users, both on the request's RLS executor.
func (b *PostgresStorageBackend) QueryUsers(ctx context.Context, q interfaces.UserQuery) ([]*interfaces.User, int, error) {
	if !q.Prefix {
		// A term the database draws no trigram from matches a prefix, as a short one does (shortSearchTerm).
		short, err := b.shortSearchTerm(ctx, strings.ToLower(q.Q))
		if err != nil {
			return nil, 0, err
		}
		q.Prefix = short
	}
	countQ, countArgs, pageQ, pageArgs := queryUsersSQL(q)
	db := b.query(ctx)
	var total int
	if err := db.QueryRowContext(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	rows, err := db.QueryContext(ctx, pageQ, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	// Never nil: an empty page is [] on the wire.
	users := []*interfaces.User{}
	for rows.Next() {
		u, err := scanListedUser(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, total, rows.Err()
}

// lockedAccountsCountSQL counts the accounts locked now, instance-wide: the dashboard's and the analytics page's
// security tiles. users_locked_until_idx (v49) holds only the accounts ever locked, so the count reads those instead of
// every account.
const lockedAccountsCountSQL = `SELECT COUNT(*)::int FROM users WHERE locked_until IS NOT NULL AND locked_until > NOW()`

// LoadUserStats counts the whole user table in one statement; "none" is what
// the four valid roles leave over. Its locked count rides that one pass over the table, which the other counts need
// anyway, so it does not use users_locked_until_idx.
func (b *PostgresStorageBackend) LoadUserStats(ctx context.Context) (*interfaces.UserStats, error) {
	var s interfaces.UserStats
	var admin, member, viewer, guest int
	err := b.query(ctx).QueryRowContext(ctx, `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE role = 'admin'),
		COUNT(*) FILTER (WHERE role = 'member'),
		COUNT(*) FILTER (WHERE role = 'viewer'),
		COUNT(*) FILTER (WHERE role = 'guest'),
		COUNT(*) FILTER (WHERE NOT email_verified),
		COUNT(*) FILTER (WHERE locked_until IS NOT NULL AND locked_until > NOW())
		FROM users`).Scan(&s.Total, &admin, &member, &viewer, &guest, &s.Unverified, &s.Locked)
	if err != nil {
		return nil, fmt.Errorf("user stats: %w", err)
	}
	s.ByRole = map[string]int{
		string(auth.RoleAdmin):  admin,
		string(auth.RoleMember): member,
		string(auth.RoleViewer): viewer,
		string(auth.RoleGuest):  guest,
		interfaces.UserRoleNone: s.Total - admin - member - viewer - guest,
	}
	return &s, nil
}

// UpdateUserRole reports an unknown id as ErrNotFound (as the filesystem backend does) instead of a
// silent no-op the admin endpoint answered with 200. A change between two roles other than admin is one statement
// whose WHERE refuses an admin row: it cannot change the number of admins, so it locks no other row. A demotion of an
// admin first locks every admin row, in id order: of two demotions at once, the second waits there, and at READ
// COMMITTED it then re-reads the rows it waited for and drops the one the first demoted (or an erasure deleted), so it
// counts the admins the first left. A count without the lock let both see two admins and both demote. FOR NO KEY
// UPDATE, not FOR UPDATE, which also waits for the key share of every foreign-key check on a users row: a role change
// queued behind any request writing a child row of an admin (a session, an Idempotency-Key claim), the admins'
// sign-ins queued behind it, and a request writing child rows of two admins deadlocked with it. A promotion takes no
// lock; one landing meanwhile only makes the count low, refusing a demotion it could have allowed.
func (b *PostgresStorageBackend) UpdateUserRole(ctx context.Context, id string, role auth.Role) error {
	if role != auth.RoleAdmin {
		res, err := b.db.ExecContext(ctx, `UPDATE users SET role = $1, updated_at = NOW() WHERE id = $2 AND role <> 'admin'`, string(role), id)
		if err != nil {
			return fmt.Errorf("update user role: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("update user role rows affected: %w", err)
		} else if n == 1 {
			return nil
		}
		// An admin, or an unknown id: the locking path below decides.
	}
	return b.withOwnTx(ctx, sql.LevelDefault, func(ctx context.Context, tx *sql.Tx) error {
		if role != auth.RoleAdmin {
			admins, err := lockAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if len(admins) == 1 && admins[0] == id {
				return interfaces.ErrLastAdmin
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET role = $1, updated_at = NOW() WHERE id = $2`, string(role), id)
		if err != nil {
			return fmt.Errorf("update user role: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update user role rows affected: %w", err)
		}
		if n == 0 {
			return interfaces.ErrNotFound
		}
		return nil
	})
}

// lockAdmins locks every admin row in id order, FOR NO KEY UPDATE (UpdateUserRole), and returns their ids.
func lockAdmins(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE role = 'admin' ORDER BY id FOR NO KEY UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("lock admins: %w", err)
	}
	defer rows.Close()
	var admins []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("lock admins: %w", err)
		}
		admins = append(admins, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock admins: %w", err)
	}
	return admins, nil
}

func (b *PostgresStorageBackend) UpdateUserPassword(ctx context.Context, id string, passwordHash string) error {
	_, err := b.db.ExecContext(ctx, `UPDATE users SET password = $1, failed_login_attempts = 0, locked_until = NULL, updated_at = NOW() WHERE id = $2`, passwordHash, id)
	return err
}

// RecordFailedLogin increments in the UPDATE, so parallel wrong passwords each count, and touches only the counter
// columns, so a reset or role change landing during the login's bcrypt survives. The lock check is repeated in the
// WHERE: an attempt that read the account before a concurrent one locked it is refused as locked, uncounted, as if
// it had read the lock, so exactly one attempt crosses the threshold.
func (b *PostgresStorageBackend) RecordFailedLogin(ctx context.Context, userID string, threshold int, lockFor time.Duration) (int, *time.Time, error) {
	var attempts int
	var lockedUntil *time.Time
	err := b.db.QueryRowContext(ctx, `
		UPDATE users SET
			failed_login_attempts = failed_login_attempts + 1,
			locked_until = CASE WHEN failed_login_attempts + 1 >= $2 THEN NOW() + $3 * INTERVAL '1 microsecond' ELSE locked_until END,
			updated_at = NOW()
		WHERE id = $1 AND (locked_until IS NULL OR locked_until <= NOW())
		RETURNING failed_login_attempts, locked_until`,
		userID, threshold, lockFor.Microseconds(),
	).Scan(&attempts, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return 0, nil, fmt.Errorf("record failed login: %w", err)
		}
		if !exists {
			return 0, nil, interfaces.ErrNotFound
		}
		return 0, nil, interfaces.ErrAccountLocked
	}
	if err != nil {
		return 0, nil, fmt.Errorf("record failed login: %w", err)
	}
	return attempts, lockedUntil, nil
}

// ResetFailedLogins checks the lock in the write that clears it, against the row as it is now (a blocked update
// re-checks the newest version): a login whose password check ran while a parallel burst locked the account is
// refused, where its earlier read said unlocked. A row already clear keeps its updated_at.
func (b *PostgresStorageBackend) ResetFailedLogins(ctx context.Context, userID string) error {
	var id string
	err := b.db.QueryRowContext(ctx, `
		UPDATE users SET failed_login_attempts = 0, locked_until = NULL,
			updated_at = CASE WHEN failed_login_attempts <> 0 OR locked_until IS NOT NULL THEN NOW() ELSE updated_at END
		WHERE id = $1 AND (locked_until IS NULL OR locked_until <= NOW())
		RETURNING id`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return fmt.Errorf("reset failed logins: %w", err)
		}
		if exists {
			return interfaces.ErrAccountLocked
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reset failed logins: %w", err)
	}
	return nil
}

// UpdateUserProfile updates the user's display name and avatar URL.
func (b *PostgresStorageBackend) UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE users SET display_name = $1, avatar_url = $2, updated_at = NOW() WHERE id = $3`,
		displayName, avatarURL, id,
	)
	if err != nil {
		return fmt.Errorf("update user profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user profile rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// UpdateUserAvatarURL is the avatar upload's and removal's write: UpdateUserProfile would need the display name, and
// one read before the write put back a name changed in between.
func (b *PostgresStorageBackend) UpdateUserAvatarURL(ctx context.Context, id string, avatarURL string) error {
	res, err := b.db.ExecContext(ctx, `UPDATE users SET avatar_url = $1, updated_at = NOW() WHERE id = $2`, avatarURL, id)
	if err != nil {
		return fmt.Errorf("update user avatar url: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user avatar url rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// ---- Uploaded avatars ----
// The write is a pool autocommit like every user write (the profile updates above); the read goes through
// b.query(ctx) so a request's row-level-security policy holds: rls_user_avatars_read admits any authenticated
// caller, because avatars are shown cross-user (comment authors, the admin list).

// SaveUserAvatar upserts the image; a missing user is the foreign key's violation, reported as ErrNotFound.
func (b *PostgresStorageBackend) SaveUserAvatar(ctx context.Context, userID, mimeType string, data []byte) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO user_avatars (user_id, mime_type, bytes, byte_size, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			mime_type  = EXCLUDED.mime_type,
			bytes      = EXCLUDED.bytes,
			byte_size  = EXCLUDED.byte_size,
			updated_at = NOW()`,
		userID, mimeType, data, len(data),
	)
	if err != nil {
		if isPgErrCode(err, pgErrForeignKeyViolation) {
			return interfaces.ErrNotFound
		}
		return fmt.Errorf("save user avatar: %w", err)
	}
	return nil
}

// LoadUserAvatar reads one user's image for the serving route; ErrNotFound when they never uploaded one.
func (b *PostgresStorageBackend) LoadUserAvatar(ctx context.Context, userID string) (*interfaces.UserAvatar, error) {
	var a interfaces.UserAvatar
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT mime_type, bytes, updated_at FROM user_avatars WHERE user_id = $1`, userID,
	).Scan(&a.MimeType, &a.Data, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user avatar: %w", err)
	}
	return &a, nil
}

// DeleteUserAvatar removes the image; a user without one is not an error.
func (b *PostgresStorageBackend) DeleteUserAvatar(ctx context.Context, userID string) error {
	if _, err := b.db.ExecContext(ctx, `DELETE FROM user_avatars WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete user avatar: %w", err)
	}
	return nil
}

// ---- Refresh-token rotation store ----
// These back the auth refresh-token rotation/revocation flow in cloud mode.

// StoreRefreshToken records an issued refresh token by its jti. Expired-row cleanup is NOT done here — the
// retention job's PurgeExpiredData runs the same indexed DELETE; inline it added a round trip to every login/refresh.
func (b *PostgresStorageBackend) StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error {
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at, user_agent, ip) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (jti) DO NOTHING`,
		jti, userID, expiresAt.UTC(), userAgent, ip,
	); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	return nil
}

// IsRefreshTokenValid reports whether the jti exists, is not revoked, and has
// not expired.
func (b *PostgresStorageBackend) IsRefreshTokenValid(ctx context.Context, jti string) (bool, error) {
	var ok bool
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM refresh_tokens
		 WHERE jti = $1 AND NOT revoked AND expires_at > NOW())`,
		jti,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check refresh token: %w", err)
	}
	return ok, nil
}

// RevokeRefreshToken revokes a single refresh token by jti (used on
// rotation). ErrTokenAlreadyRevoked lets callers detect a concurrent
// rotation race.
func (b *PostgresStorageBackend) RevokeRefreshToken(ctx context.Context, jti string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1 AND NOT revoked`, jti,
	)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke refresh token: rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrTokenAlreadyRevoked
	}
	return nil
}

// VerifyAndRevokeRefreshToken atomically verifies and revokes a refresh
// token, eliminating the race window between VerifyRefresh and
// RevokeRefreshToken. ErrTokenAlreadyRevoked if already revoked/invalid.
func (b *PostgresStorageBackend) VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*interfaces.RefreshTokenInfo, error) {
	var info interfaces.RefreshTokenInfo
	err := b.db.QueryRowContext(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE
		WHERE jti = $1
		  AND NOT revoked
		  AND expires_at > NOW()
		RETURNING jti, user_id, created_at, expires_at`,
		jti,
	).Scan(&info.ID, &info.UserID, &info.CreatedAt, &info.ExpiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrTokenAlreadyRevoked
		}
		return nil, fmt.Errorf("verify and revoke refresh token: %w", err)
	}
	return &info, nil
}

// RevokeUserRefreshTokens revokes every refresh token for a user (used on logout).
func (b *PostgresStorageBackend) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE user_id = $1 AND NOT revoked`, userID,
	); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}

// ListUserRefreshTokens returns every active (non-revoked, non-expired)
// refresh token for a user, surfaced to the user as "active sessions".
func (b *PostgresStorageBackend) ListUserRefreshTokens(ctx context.Context, userID string) ([]interfaces.RefreshTokenInfo, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT jti, created_at, expires_at, user_agent, ip FROM refresh_tokens
		 WHERE user_id = $1 AND NOT revoked AND expires_at > NOW()
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list user refresh tokens: %w", err)
	}
	defer rows.Close()

	var sessions []interfaces.RefreshTokenInfo
	for rows.Next() {
		var s interfaces.RefreshTokenInfo
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt, &s.UserAgent, &s.IP); err != nil {
			return nil, fmt.Errorf("scan refresh token: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// RevokeRefreshTokenForUser revokes a single refresh token by jti, but only
// if it belongs to userID — a user can revoke their own sessions without
// being able to revoke another's by guessing a jti.
func (b *PostgresStorageBackend) RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1 AND user_id = $2 AND NOT revoked`,
		jti, userID,
	)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke refresh token rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// ---- Audit log ----

func (b *PostgresStorageBackend) SaveAuditEvent(ctx context.Context, event *interfaces.AuditEvent) error {
	meta, err := json.Marshal(event.Meta)
	if err != nil {
		meta = []byte("{}")
	}
	_, err = b.db.ExecContext(ctx,
		`INSERT INTO audit_events (`+auditInsertColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''))`+auditInsertOnConflict,
		auditInsertArgs(event, meta)...,
	)
	if err != nil {
		return fmt.Errorf("save audit event: %w", err)
	}
	return nil
}

// auditInsertColumns is the column list of every audit_events INSERT; the v33 columns are written as NULL when
// empty, like the rows written before them.
const auditInsertColumns = "id, user_id, email, action, resource_type, resource_id, ip, meta, created_at, " +
	"org_id, actor_kind, token_id, actor_job, user_agent"

// auditInsertOnConflict makes every audit INSERT idempotent: an event is written at least once (a spill file is
// replayed after a crash between the insert and the file rewrite, or adopted by another replica), and the first write
// of an id stays.
const auditInsertOnConflict = " ON CONFLICT (id) DO NOTHING"

// auditInsertArgs is the bind arguments of one row, in auditInsertColumns order.
func auditInsertArgs(e *interfaces.AuditEvent, meta []byte) []any {
	return []any{e.ID, e.UserID, e.Email, e.Action, e.ResourceType, e.ResourceID, e.IP, meta, e.CreatedAt,
		e.OrgID, e.ActorKind, e.TokenID, e.ActorJob, e.UserAgent}
}

// SaveAuditEvents inserts a batch of audit events in a single round-trip — the async audit worker's
// throughput path (a single bad row failing the whole batch is handled by the caller falling back to
// per-event writes). auditInsertCols is the number of columns (and bind params per row) in the audit_events
// INSERT: Postgres caps a single statement at 65535 bind parameters, so batches must stay under 65535/14 ≈ 4681 rows.
const auditInsertCols = 14

// auditInsertBatchRows bounds one INSERT well under the 65535-param ceiling
// (4000*14 = 56000 params).
const auditInsertBatchRows = 4000

// chunkAuditEvents splits events into contiguous batches of at most size rows.
// A size <= 0 yields a single batch (no chunking).
func chunkAuditEvents(events []*interfaces.AuditEvent, size int) [][]*interfaces.AuditEvent {
	if size <= 0 || len(events) <= size {
		return [][]*interfaces.AuditEvent{events}
	}
	out := make([][]*interfaces.AuditEvent, 0, (len(events)+size-1)/size)
	for start := 0; start < len(events); start += size {
		end := min(start+size, len(events))
		out = append(out, events[start:end])
	}
	return out
}

// buildAuditInsert renders a multi-row INSERT for one batch (callers must
// keep len(batch) <= auditInsertBatchRows).
func buildAuditInsert(batch []*interfaces.AuditEvent) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(batch)*auditInsertCols)
	sb.WriteString(`INSERT INTO audit_events (` + auditInsertColumns + `) VALUES `)
	for i, e := range batch {
		meta, err := json.Marshal(e.Meta)
		if err != nil {
			meta = []byte("{}")
		}
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*auditInsertCols + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,NULLIF($%d,''),NULLIF($%d,''),NULLIF($%d,''),NULLIF($%d,''),NULLIF($%d,''))",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
			base+9, base+10, base+11, base+12, base+13)
		args = append(args, auditInsertArgs(e, meta)...)
	}
	sb.WriteString(auditInsertOnConflict)
	return sb.String(), args
}

func (b *PostgresStorageBackend) SaveAuditEvents(ctx context.Context, events []*interfaces.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	// Chunk larger batches and run every chunk in one transaction so the
	// whole write stays all-or-nothing.
	err := b.withOwnTx(ctx, sql.LevelDefault, func(_ context.Context, tx *sql.Tx) error {
		for _, batch := range chunkAuditEvents(events, auditInsertBatchRows) {
			query, args := buildAuditInsert(batch)
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save audit events (%d): %w", len(events), err)
	}
	return nil
}

// ListAuditEvents page bounds. maxAuditLimit is package-level so the data-subject export
// (exportAuditEvents) pages by exactly the clamp.
const (
	defaultAuditLimit = 100
	maxAuditLimit     = 500
)

func (b *PostgresStorageBackend) ListAuditEvents(ctx context.Context, filter interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) {
	var after *auditCursor
	if a := filter.After; a != nil {
		after = &auditCursor{createdAt: a.At, id: a.ID}
	}
	return b.listAuditEvents(ctx, filter, after)
}

// auditCursor is a keyset position in the audit log's order (created_at
// DESC, id ASC): the last row a walk has seen.
type auditCursor struct {
	createdAt time.Time
	id        string
}

// auditEventColumns is the audit_events select list every variant of the
// list statement reads, in the order scanAuditEvents expects.
const auditEventColumns = "id, user_id, email, action, resource_type, resource_id, ip, meta, created_at, " +
	"org_id, actor_kind, token_id, actor_job, user_agent"

// auditEventOrder orders the audit log newest first, with `id` (the primary key) as the final tiebreaker:
// events logged in the same microsecond tie on created_at and SQL orders tied rows arbitrarily, so paging could
// drop or repeat records — for a security audit trail "I paged through the log and it wasn't there" must be sound.
const auditEventOrder = "ORDER BY created_at DESC, id ASC"

// listAuditEventsSQL builds the ListAuditEvents statement for filter, with its page bounds already clamped.
// The ORDER BY is auditEventOrder in every shape; the LIMIT/OFFSET page is served from index-ordered walks:
// UserID alone reads audit_events_user_created_idx; Action alone reads audit_events_action_created_idx (v28).
// UserID with IncludeSubject: pre-auth events (sign-in, lockout, registration, SSO) carry no actor — their
// subject is the user in resource_type/resource_id — so the two populations are read as two arms of a UNION ALL,
// each from its own index in the list's order, cut to the first LIMIT+OFFSET rows, then merged and paged (the
// single-OR shape this replaces was a BitmapOr plus a Sort of the user's whole history for every page);
// `user_id <> $1` on the subject arm keeps an event whose actor IS its subject in the actor arm only.
// after, when set, replaces OFFSET paging with a keyset continuation — rows strictly after
// (after.createdAt, after.id) — which the data-subject export uses to walk a user's whole history, and the admin log
// since v49 (AuditFilter.After): every page costs one index descent instead of re-reading every earlier page. The
// OFFSET shape stays for the admin page's older clients; there, events written WHILE an operator pages shift later
// rows down and the boundary skips one per new event, which the cursor does not.
func listAuditEventsSQL(filter interfaces.AuditFilter, limit, offset int, after *auditCursor) (string, []any) {
	args := sqlArgs{}
	// Predicates every arm carries, after the arm's own key.
	common := []string{}
	userArg := ""
	if filter.UserID != "" {
		userArg = args.add(filter.UserID)
	}
	if filter.Action != "" {
		common = append(common, "action = "+args.add(filter.Action))
	}
	if filter.Since != nil {
		common = append(common, "created_at >= "+args.add(filter.Since.UTC()))
	}
	if after != nil {
		at, id := args.add(after.createdAt.UTC()), args.add(after.id)
		common = append(common, "created_at <= "+at+" AND (created_at < "+at+" OR id > "+id+")")
		offset = 0
	}
	where := func(key ...string) string {
		preds := append(key, common...)
		if len(preds) == 0 {
			return ""
		}
		return " WHERE " + strings.Join(preds, " AND ")
	}

	// #nosec G202 -- only generated "$N" placeholders and whitelisted column
	// names are concatenated; all values are parameterized args.
	var q string
	switch {
	case userArg != "" && filter.IncludeSubject:
		// Each arm keeps the first LIMIT+OFFSET rows of its own ordered walk:
		// any row of the final page is within that prefix of its arm.
		armLimit := args.add(limit + offset)
		q = "SELECT " + auditEventColumns + " FROM (" +
			"(SELECT " + auditEventColumns + " FROM audit_events" + where("user_id = "+userArg) + " " + auditEventOrder + " LIMIT " + armLimit + ")" +
			" UNION ALL " +
			"(SELECT " + auditEventColumns + " FROM audit_events" + where("resource_type = 'user' AND resource_id = "+userArg+" AND user_id <> "+userArg) + " " + auditEventOrder + " LIMIT " + armLimit + ")" +
			") u"
	case userArg != "":
		q = "SELECT " + auditEventColumns + " FROM audit_events" + where("user_id = "+userArg)
	default:
		q = "SELECT " + auditEventColumns + " FROM audit_events" + where()
	}
	// The page binds last, after every predicate and the arms' limit.
	q += " " + auditEventOrder + " LIMIT " + args.add(limit)
	if after == nil {
		q += " OFFSET " + args.add(offset)
	}
	return q, args
}

// listAuditEvents is ListAuditEvents with an optional keyset continuation
// (see listAuditEventsSQL).
func (b *PostgresStorageBackend) listAuditEvents(ctx context.Context, filter interfaces.AuditFilter, after *auditCursor) ([]*interfaces.AuditEvent, error) {
	// A cap clamps DOWN to the maximum; it does not fall back to the default. (The old form read
	// `if limit <= 0 || limit > 500 { limit = 100 }`, so asking for MORE than the maximum silently
	// returned five times FEWER — the GDPR export asked for 1000 and shipped exactly 100, no error.)
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditLimit
	case limit > maxAuditLimit:
		limit = maxAuditLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	q, args := listAuditEventsSQL(filter, limit, offset, after)
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var events []*interfaces.AuditEvent
	for rows.Next() {
		ev := &interfaces.AuditEvent{}
		var metaRaw []byte
		var orgID, actorKind, tokenID, actorJob, userAgent sql.NullString
		if err := rows.Scan(&ev.ID, &ev.UserID, &ev.Email, &ev.Action,
			&ev.ResourceType, &ev.ResourceID, &ev.IP, &metaRaw, &ev.CreatedAt,
			&orgID, &actorKind, &tokenID, &actorJob, &userAgent); err != nil {
			return nil, err
		}
		ev.OrgID, ev.ActorKind, ev.TokenID = orgID.String, actorKind.String, tokenID.String
		ev.ActorJob, ev.UserAgent = actorJob.String, userAgent.String
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &ev.Meta); err != nil {
				slog.Warn("audit event: failed to unmarshal metadata", "eventID", ev.ID, "error", err)
			}
		}
		events = append(events, ev)
	}
	if events == nil {
		events = []*interfaces.AuditEvent{}
	}
	return events, rows.Err()
}

// ---- Usage metrics ----

func (b *PostgresStorageBackend) SaveUsageMetric(ctx context.Context, metric *interfaces.UsageMetric) error {
	query := `
		INSERT INTO usage_metrics (id, user_id, org_id, provider, model, served_model, prompt_tokens, completion_tokens, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := b.db.ExecContext(ctx, query,
		metric.ID, metric.UserID, metric.OrgID, metric.Provider, metric.Model, metric.ServedModel,
		metric.PromptTokens, metric.CompletionTokens, metric.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert usage_metric: %w", err)
	}
	return nil
}
