package interfaces

import (
	"context"
	"errors"
	"io"
	"time"

	"pad-analyzer/internal/auth"
)

// ErrEmailExists is returned by CreateUser for a duplicate email (map to HTTP 409).
var ErrEmailExists = errors.New("email already in use")

// ErrAccountLocked is RecordFailedLogin refusing to count an attempt against an account locked since it was read.
var ErrAccountLocked = errors.New("account is locked")

// ErrLastAdmin is UpdateUserRole refusing to take the role from the deployment's last admin.
var ErrLastAdmin = errors.New("the last administrator keeps the role")

// ErrUserIDRequired is DeleteUser refusing the empty id, which names no account but the rows of local mode and of
// erased members.
var ErrUserIDRequired = errors.New("a user id is required")

// UserAvatar is a user's uploaded avatar image: the re-encoded bytes the upload path produced (PNG or JPEG, never
// the file as uploaded), their type, and the updated stamp the serving route's ETag comes from.
type UserAvatar struct {
	MimeType  string    `json:"mimeType"`
	Data      []byte    `json:"data"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// User represents a system user in the storage backend.
type User struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	EmailVerified       bool       `json:"emailVerified"`
	FailedLoginAttempts int        `json:"-"`
	LockedUntil         *time.Time `json:"-"`
	Password            string     `json:"-"` // Bcrypt hash — never serialized to clients
	Role                auth.Role  `json:"role"`
	DisplayName         string     `json:"displayName"`
	AvatarURL           string     `json:"avatarUrl"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

// UserStore covers user account CRUD, role/password/profile updates, and the GDPR bulk operations (export, erasure, retention purge).
type UserStore interface {
	// SaveUser writes every column of user, for seeding and tests: a live account changes through the targeted
	// updates below, since writing back a row read earlier undoes whatever changed in between.
	SaveUser(ctx context.Context, user *User) error
	CreateUser(ctx context.Context, user *User) error
	LoadUserByEmail(ctx context.Context, email string) (*User, error)
	LoadUserByID(ctx context.Context, id string) (*User, error)
	// LoadUsersByIDs resolves many users in one round trip; missing IDs are absent from the map (avoids N+1).
	LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*User, error)
	CountUsers(ctx context.Context) (int, error)
	// ListUsers returns up to limit users from offset, created_at desc; non-positive limit returns all.
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	ListAdmins(ctx context.Context) ([]*User, error)
	// QueryUsers returns one page of the admin user list (q's filters, created_at desc then id, q.Offset/q.Limit) plus the total; page never nil.
	QueryUsers(ctx context.Context, q UserQuery) ([]*User, int, error)
	// LoadUserStats counts the whole user table by role and state for the admin overview.
	LoadUserStats(ctx context.Context) (*UserStats, error)
	// UpdateUserRole sets the role; ErrNotFound for an unknown id. It never takes the role from the deployment's last
	// admin (ErrLastAdmin), decided atomically with every other role change: two admins demoting each other at once
	// leave one.
	UpdateUserRole(ctx context.Context, id string, role auth.Role) error
	UpdateUserPassword(ctx context.Context, id string, passwordHash string) error
	// RecordFailedLogin counts one wrong password in a single write: the count goes up by one and, once it reaches
	// threshold, the account locks until now plus lockFor. It returns the stored count and lock. An account locked
	// at the time of the write is not counted (ErrAccountLocked); an unknown id is ErrNotFound.
	RecordFailedLogin(ctx context.Context, userID string, threshold int, lockFor time.Duration) (attempts int, lockedUntil *time.Time, err error)
	// ResetFailedLogins clears the failed-login count and lock and nothing else; a no-op when both are clear or the
	// id is unknown. An account locked at the time of the write is left as it is and is ErrAccountLocked: a login
	// that read it unlocked before a parallel attempt locked it does not get through.
	ResetFailedLogins(ctx context.Context, userID string) error
	// UpdateUserProfile updates the display name and avatar URL; an empty value clears it.
	UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error
	// UpdateUserAvatarURL sets the avatar URL alone (empty clears it), leaving the display name to its own writer;
	// ErrNotFound for an unknown id.
	UpdateUserAvatarURL(ctx context.Context, id string, avatarURL string) error
	// SaveUserAvatar upserts the user's uploaded avatar image (already validated and re-encoded); ErrNotFound when
	// the user does not exist.
	SaveUserAvatar(ctx context.Context, userID, mimeType string, data []byte) error
	// LoadUserAvatar reads any user's avatar for serving; ErrNotFound when they never uploaded one.
	LoadUserAvatar(ctx context.Context, userID string) (*UserAvatar, error)
	// DeleteUserAvatar removes the uploaded image; idempotent — a user without one is not an error.
	DeleteUserAvatar(ctx context.Context, userID string) error

	// DeleteUser performs GDPR erasure: deletes owned flows and per-user rows, anonymizes PII on shared rows (forensic trail retained); idempotent — a missing user is not an error. The empty id is ErrUserIDRequired.
	DeleteUser(ctx context.Context, userID string) error
	// ExportUserData assembles the data-subject export; ErrNotFound if the user does not exist.
	ExportUserData(ctx context.Context, userID string) (*UserDataExport, error)
	// ExportUserDataTo writes the same export to w as one JSON object, without holding every flow in memory; ErrNotFound if the user does not exist. After an error w holds partial JSON.
	ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error
	// PurgeExpiredData removes retention-elapsed rows (expired tokens/invites/idempotency keys, audit_events older than auditRetentionDays — 0 keeps audit forever); returns removal counts.
	PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*PurgeResult, error)
}
