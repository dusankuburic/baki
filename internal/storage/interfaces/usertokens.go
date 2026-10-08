package interfaces

import (
	"context"
	"time"
)

// UserTokenStore covers one-shot user tokens (password reset, email verification); only the hash is stored.
type UserTokenStore interface {
	CreateUserToken(ctx context.Context, t *UserToken) error
	// ConsumeUserToken atomically redeems a valid (unused, unexpired) token of the purpose, marks it used, and returns the owning userID (ErrNotFound — no replay).
	ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (userID string, err error)
	// InvalidateUserTokens marks every unused, unexpired token of the purposes used, so outstanding reset/verify links die after a password change (account-takeover guard); no-op when none match.
	InvalidateUserTokens(ctx context.Context, userID string, purposes ...string) error
	// SetUserEmailVerified marks a user's email as verified.
	SetUserEmailVerified(ctx context.Context, userID string) error
}

// UserToken purposes.
const (
	TokenPurposePasswordReset = "password_reset"
	TokenPurposeEmailVerify   = "email_verify"
)

// UserToken is a single-use credential for an out-of-band flow (password reset or email verification); stored hashed, redeemed via ConsumeUserToken.
type UserToken struct {
	TokenHash string    `json:"-"`
	Purpose   string    `json:"purpose"`
	UserID    string    `json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
