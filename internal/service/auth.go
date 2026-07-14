package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/auth"
	mailer "pad-analyzer/internal/mail"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// Password reset and email verification token lifetimes.
const (
	PasswordResetTTL = time.Hour
	EmailVerifyTTL   = 24 * time.Hour
)

// failedLoginLockThreshold / accountLockDuration govern the login lockout
// policy: an account is locked for accountLockDuration once it accumulates
// failedLoginLockThreshold consecutive failed attempts.
const (
	failedLoginLockThreshold = 5
	accountLockDuration      = 15 * time.Minute
)

// dummyBcryptHash is checked against an incoming password when no matching
// user exists, so a login attempt for a non-existent account takes the same
// time as one for a real account with a wrong password. Without this, a
// missing-user response returns in ~1ms (no bcrypt) while a real account's
// wrong-password response takes ~50ms (bcrypt runs), letting an attacker
// enumerate valid emails purely from response timing.
const dummyBcryptHash = "$2a$12$R.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v.8j9.v"

var (
	// ErrInvalidOldPassword is returned by ChangePassword when oldPassword
	// doesn't match the account's current password.
	ErrInvalidOldPassword = errors.New("invalid old password")
	// ErrUserLookupFailed wraps a LoadUserByID failure in ChangePassword — kept
	// distinct from other failures so the caller can map it to 404 the same way
	// the original inline handler did (unconditionally, for any lookup error).
	ErrUserLookupFailed = errors.New("user lookup failed")
	// ErrPasswordHashFailed wraps a bcrypt hashing failure in Register, kept
	// distinct from CreateUser failures so the caller can return 500 instead of
	// the 409 used for a duplicate-email conflict.
	ErrPasswordHashFailed = errors.New("password hashing failed")
	// ErrInvalidResetToken / ErrInvalidVerifyToken are returned by
	// ResetPassword / VerifyEmail for any token-redemption failure (expired,
	// already used, or simply unknown) — the underlying storage error is
	// deliberately not surfaced, since it isn't meaningful to the caller and
	// (for an unknown token) could hint at what exists.
	ErrInvalidResetToken  = errors.New("invalid or expired reset token")
	ErrInvalidVerifyToken = errors.New("invalid or expired verification token")
)

// authStore is the narrow slice of StorageBackend AuthService needs: account
// CRUD/lookup plus the one-shot tokens backing password-reset/email-verify.
type authStore interface {
	storageif.UserStore
	storageif.UserTokenStore
}

// AuthService owns the credential-verification and account-mutation business
// logic that previously lived inline in handlers_auth.go: bcrypt timing-attack
// mitigation, failed-login lockout, password hashing/change, and the
// token-redemption flows for password reset and email verification. JWT
// issuance and refresh-token/session bookkeeping stay in AuthHandler — those
// are thin delegations to the already-well-abstracted auth.Manager and
// RefreshTokenStore, not business logic that benefits from a service seam.
type AuthService struct {
	backend authStore
	// email may be nil in tests that don't exercise the notification paths;
	// callers must nil-check before use (mirrors mailer.Service's own
	// Enabled() gate for a log-only fallback).
	email *mailer.Service
}

func NewAuthService(backend authStore, email *mailer.Service) *AuthService {
	return &AuthService{backend: backend, email: email}
}

// Register hashes the password and creates the account. CreateUser atomically
// promotes the very first user in the system to RoleAdmin.
func (s *AuthService) Register(ctx context.Context, email, password string) (*storageif.User, error) {
	hashed, err := auth.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPasswordHashFailed, err)
	}
	user := &storageif.User{
		ID:       uuid.NewString(),
		Email:    email,
		Password: hashed,
		Role:     auth.RoleMember,
	}
	if err := s.backend.CreateUser(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// SendVerificationEmail issues an email-verification token and mails the link.
// Best-effort: failures are logged but never returned, since email is optional
// and a user can request a resend later.
func (s *AuthService) SendVerificationEmail(ctx context.Context, user *storageif.User) {
	if s.email == nil || user.Email == "" {
		return
	}
	raw, hash, err := auth.GenerateOpaqueToken()
	if err != nil {
		logger.Error("verification token generation failed", "error", err, "userID", user.ID)
		return
	}
	tok := &storageif.UserToken{
		TokenHash: hash,
		Purpose:   storageif.TokenPurposeEmailVerify,
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(EmailVerifyTTL),
	}
	if err := s.backend.CreateUserToken(ctx, tok); err != nil {
		logger.Error("storing verification token failed", "error", err, "userID", user.ID)
		return
	}
	if err := s.email.SendEmailVerification(ctx, user.Email, raw); err != nil {
		logger.Error("sending verification email failed", "error", err, "userID", user.ID)
	}
}

// LoginOutcome distinguishes why Authenticate failed (or that it succeeded),
// so the caller can log the correct audit reason and resource identifier
// without re-deriving any of Authenticate's internal, timing-sensitive logic.
type LoginOutcome int

const (
	LoginSuccess LoginOutcome = iota
	LoginUserNotFound
	LoginAccountLocked
	LoginInvalidPassword
	// LoginEmailNotVerified indicates the credentials were correct but the
	// account's email has not been verified. Checked AFTER the password so a
	// stranger can't learn whether an unverified account exists (they get
	// "invalid credentials" regardless); an attacker who registered a victim's
	// email and set a known password is blocked from using the shadow account
	// until they complete verification (which they can't, since they don't
	// control the inbox).
	LoginEmailNotVerified
)

// LoginResult is returned by Authenticate for every outcome (including
// failures), so the caller always has enough information to build an accurate
// audit event.
type LoginResult struct {
	Outcome LoginOutcome
	// User is set whenever a matching account was found — every outcome
	// except LoginUserNotFound — even on failure, so the caller can
	// audit-log against the correct user ID.
	User *storageif.User
	// AccountJustLocked is true when THIS call pushed the account over the
	// failed-attempt threshold and newly locked it (vs. an already-locked
	// account) — the caller uses it to decide whether to also emit the
	// separate "account locked" audit event.
	AccountJustLocked bool
}

// Authenticate verifies an email+password pair with the timing-attack
// mitigations this endpoint requires:
//
//   - A non-existent user still runs a (dummy) bcrypt check before returning,
//     so response time can't be used to enumerate accounts (bcrypt is
//     expensive; an early return would make a missing-user response
//     measurably faster than a real one).
//   - For an existing user, the REAL bcrypt check runs BEFORE the lock-status
//     check, so a locked account (fast: DB lookup only) can't be
//     timing-distinguished from an unlocked one with a wrong password (slow:
//     DB + bcrypt).
//
// Do not reorder these checks.
//
// On a wrong password it increments the failed-attempt counter and locks the
// account for accountLockDuration once failedLoginLockThreshold is reached; on
// success it resets the counter. Both updates are best-effort — a SaveUser
// failure is logged but does not fail the login/lockout decision itself.
func (s *AuthService) Authenticate(ctx context.Context, email, password string) LoginResult {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil {
		auth.CheckPasswordHash(password, dummyBcryptHash)
		return LoginResult{Outcome: LoginUserNotFound}
	}

	passwordValid := auth.CheckPasswordHash(password, user.Password)

	if user.LockedUntil != nil && time.Now().UTC().Before(*user.LockedUntil) {
		return LoginResult{Outcome: LoginAccountLocked, User: user}
	}

	if !passwordValid {
		user.FailedLoginAttempts++
		justLocked := false
		if user.FailedLoginAttempts >= failedLoginLockThreshold {
			until := time.Now().UTC().Add(accountLockDuration)
			user.LockedUntil = &until
			justLocked = true
			logger.Warn("account locked due to too many failed login attempts", "email", user.Email, "userID", user.ID)
		}
		if err := s.backend.SaveUser(ctx, user); err != nil {
			logger.Error("failed to update failed login attempts", "error", err, "userID", user.ID)
		}
		return LoginResult{Outcome: LoginInvalidPassword, User: user, AccountJustLocked: justLocked}
	}

	if user.FailedLoginAttempts > 0 || user.LockedUntil != nil {
		user.FailedLoginAttempts = 0
		user.LockedUntil = nil
		if err := s.backend.SaveUser(ctx, user); err != nil {
			logger.Error("failed to reset failed login attempts", "error", err, "userID", user.ID)
		}
	}

	// The password is correct, but the account's email is unverified. This
	// blocks the shadow-registration takeover: an attacker who registered a
	// victim's email and knows the password still can't get a session. Placed
	// after the failed-attempt reset so a correct password doesn't accumulate
	// lockout pressure.
	if !user.EmailVerified {
		return LoginResult{Outcome: LoginEmailNotVerified, User: user}
	}

	return LoginResult{Outcome: LoginSuccess, User: user}
}

// ChangePassword verifies oldPassword against the account's current hash,
// then hashes and stores newPassword. It invalidates any outstanding
// password-reset / email-verify tokens (best-effort — a failure is logged, not
// returned) so a user who changes their password through the logged-in UI
// can't remain recoverable via a previously-issued reset link.
func (s *AuthService) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	user, err := s.backend.LoadUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUserLookupFailed, err)
	}
	if !auth.CheckPasswordHash(oldPassword, user.Password) {
		return ErrInvalidOldPassword
	}
	hashed, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.backend.UpdateUserPassword(ctx, user.ID, hashed); err != nil {
		return err
	}
	if err := s.backend.InvalidateUserTokens(ctx, user.ID,
		storageif.TokenPurposePasswordReset, storageif.TokenPurposeEmailVerify); err != nil {
		logger.Error("failed to invalidate outstanding reset/verify tokens after password change", "error", err, "userID", user.ID)
	}
	return nil
}

// RequestPasswordReset issues a password-reset token and emails it, for a
// known account. Returns (nil, nil) — NOT an error — when email doesn't match
// a user, so the caller can respond identically to the success case
// (anti-enumeration: an unknown address must not be distinguishable from a
// known one by response shape).
func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) (*storageif.User, error) {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil {
		return nil, nil
	}
	raw, hash, err := auth.GenerateOpaqueToken()
	if err != nil {
		return user, err
	}
	tok := &storageif.UserToken{
		TokenHash: hash,
		Purpose:   storageif.TokenPurposePasswordReset,
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(PasswordResetTTL),
	}
	if err := s.backend.CreateUserToken(ctx, tok); err != nil {
		return user, err
	}
	if s.email != nil {
		if err := s.email.SendPasswordReset(ctx, user.Email, raw); err != nil {
			logger.Error("sending password reset email failed", "error", err, "userID", user.ID)
		}
	}
	return user, nil
}

// ResetPassword redeems a password-reset token and sets a new password.
func (s *AuthService) ResetPassword(ctx context.Context, token, newPassword string) (userID string, err error) {
	userID, err = s.backend.ConsumeUserToken(ctx, storageif.TokenPurposePasswordReset, auth.HashOpaqueToken(token))
	if err != nil {
		return "", ErrInvalidResetToken
	}
	hashed, err := auth.HashPassword(newPassword)
	if err != nil {
		return "", err
	}
	if err := s.backend.UpdateUserPassword(ctx, userID, hashed); err != nil {
		return "", err
	}
	// Revoke all other outstanding reset / verify tokens for this user so a
	// previously-issued (and possibly leaked) reset link can't be redeemed
	// after the account has already been recovered.
	if err := s.backend.InvalidateUserTokens(ctx, userID,
		storageif.TokenPurposePasswordReset, storageif.TokenPurposeEmailVerify); err != nil {
		logger.Error("failed to invalidate outstanding reset/verify tokens after reset", "error", err, "userID", userID)
	}
	return userID, nil
}

// VerifyEmail redeems an email-verification token and marks the account's
// email verified.
func (s *AuthService) VerifyEmail(ctx context.Context, token string) (userID string, err error) {
	userID, err = s.backend.ConsumeUserToken(ctx, storageif.TokenPurposeEmailVerify, auth.HashOpaqueToken(token))
	if err != nil {
		return "", ErrInvalidVerifyToken
	}
	if err := s.backend.SetUserEmailVerified(ctx, userID); err != nil {
		return "", err
	}
	return userID, nil
}
