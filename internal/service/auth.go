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

// failedLoginLockThreshold / accountLockDuration govern the lockout policy: an account locks for the duration
// once failedLoginLockThreshold consecutive attempts accumulate.
const (
	failedLoginLockThreshold = 5
	accountLockDuration      = 15 * time.Minute
)

var (
	// ErrInvalidOldPassword is returned by ChangePassword for a wrong oldPassword.
	ErrInvalidOldPassword = errors.New("invalid old password")
	// ErrUserLookupFailed wraps a LoadUserByID failure in ChangePassword, so the caller can map it to 404.
	ErrUserLookupFailed = errors.New("user lookup failed")
	// ErrPasswordHashFailed wraps a bcrypt hashing failure in Register, so the caller returns 500, not the 409
	// used for a duplicate-email conflict.
	ErrPasswordHashFailed = errors.New("password hashing failed")
	// ErrInvalidResetToken / ErrInvalidVerifyToken are returned for any token-redemption failure (expired,
	// already used, unknown) — the storage error is deliberately not surfaced: it could hint at what exists.
	ErrInvalidResetToken  = errors.New("invalid or expired reset token")
	ErrInvalidVerifyToken = errors.New("invalid or expired verification token")
)

// authStore is the narrow slice of StorageBackend AuthService needs: account CRUD/lookup plus one-shot tokens.
type authStore interface {
	storageif.UserStore
	storageif.UserTokenStore
}

// AuthService owns the credential-verification and account-mutation business logic: bcrypt timing-attack
// mitigation, failed-login lockout, password hashing/change, and token redemption for reset/verify.
// JWT issuance and refresh-token bookkeeping stay in AuthHandler — thin delegations, not business logic.
type AuthService struct {
	backend authStore
	// email may be nil in tests; callers must nil-check before use.
	email *mailer.Service
}

func NewAuthService(backend authStore, email *mailer.Service) *AuthService {
	return &AuthService{backend: backend, email: email}
}

// Register hashes the password and creates the account; CreateUser atomically promotes the very first user to
// RoleAdmin (this path opts into the bootstrap rule; SSO JIT does not, so the admin is the first registered user).
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
	if err := s.backend.CreateUser(auth.WithAllowBootstrap(ctx, true), user); err != nil {
		return nil, err
	}
	return user, nil
}

// SendVerificationEmail issues an email-verification token and mails the link; best-effort, never an error.
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

// ResendVerification re-issues the verification link for a known, still-unverified account. Returns (nil, nil) —
// NOT an error — for unknown or already-verified addresses, so the caller responds identically in every case
// (anti-enumeration). Outstanding tokens are invalidated first so only the newest link redeems.
func (s *AuthService) ResendVerification(ctx context.Context, email string) (*storageif.User, error) {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil || user == nil || user.EmailVerified {
		return nil, nil
	}
	if err := s.backend.InvalidateUserTokens(ctx, user.ID, storageif.TokenPurposeEmailVerify); err != nil {
		logger.Error("failed to invalidate outstanding verification tokens before resend", "error", err, "userID", user.ID)
	}
	s.SendVerificationEmail(ctx, user)
	return user, nil
}

// LoginOutcome distinguishes why Authenticate failed, so the caller logs the correct audit reason without
// re-deriving timing-sensitive logic.
type LoginOutcome int

const (
	LoginSuccess LoginOutcome = iota
	LoginUserNotFound
	LoginAccountLocked
	LoginInvalidPassword
	// LoginEmailNotVerified indicates correct credentials but an unverified email. Checked AFTER the password
	// so a stranger can't learn whether an unverified account exists ("invalid credentials" regardless); an
	// attacker who registered a victim's email with a known password is blocked until verification (which they
	// can't complete without the inbox).
	LoginEmailNotVerified
)

// LoginResult is returned by Authenticate for every outcome (including failures), enough for an accurate audit event.
type LoginResult struct {
	Outcome LoginOutcome
	// User is set whenever a matching account was found — even on failure, so the caller audits the correct ID.
	User *storageif.User
	// AccountJustLocked is true when THIS call newly locked the account (vs. already locked) — the caller uses
	// it to decide whether to also emit the "account locked" audit event.
	AccountJustLocked bool
}

// Authenticate verifies an email+password pair with the timing-attack mitigations this endpoint requires:
//
//   - A non-existent user still runs a (dummy) bcrypt check before returning, so response time can't enumerate
//     accounts (bcrypt is expensive; an early return would make a missing-user response measurably faster).
//   - For an existing user, the REAL bcrypt check runs BEFORE the lock-status check, so a locked account (fast:
//     DB only) can't be timing-distinguished from an unlocked one with a wrong password (slow: DB + bcrypt).
//
// Do not reorder these checks.
//
// On a wrong password it increments the counter and locks at failedLoginLockThreshold; on success it resets it.
// Both updates are best-effort — a SaveUser failure is logged but does not fail the login/lockout decision.
func (s *AuthService) Authenticate(ctx context.Context, email, password string) LoginResult {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil {
		// A login for a non-existent account still runs a bcrypt check against a dummy hash: without it a
		// missing-user response returns in ~1 ms vs ~0.2 s for a real one, enumerating emails by timing.
		auth.CheckDummyPassword(password)
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

	// Correct password, unverified email: blocks the shadow-registration takeover — an attacker who registered
	// a victim's email with a known password still can't get a session. Placed after the reset so a correct
	// password doesn't accumulate lockout pressure.
	if !user.EmailVerified {
		return LoginResult{Outcome: LoginEmailNotVerified, User: user}
	}

	return LoginResult{Outcome: LoginSuccess, User: user}
}

// ChangePassword verifies oldPassword, then hashes and stores newPassword. Outstanding reset/verify tokens are
// invalidated (best-effort) so a previously-issued reset link can't recover the account afterwards.
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

// RequestPasswordReset issues a reset token and emails it. Returns (nil, nil) — NOT an error — when email
// doesn't match a user, so the caller responds identically (anti-enumeration).
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
	// Revoke other outstanding reset/verify tokens so a possibly-leaked link can't be redeemed after recovery.
	if err := s.backend.InvalidateUserTokens(ctx, userID,
		storageif.TokenPurposePasswordReset, storageif.TokenPurposeEmailVerify); err != nil {
		logger.Error("failed to invalidate outstanding reset/verify tokens after reset", "error", err, "userID", userID)
	}
	return userID, nil
}

// VerifyEmail redeems an email-verification token and marks the email verified.
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
