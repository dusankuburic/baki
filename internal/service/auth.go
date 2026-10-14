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

// authStore is the narrow slice of StorageBackend AuthService needs: account CRUD/lookup, one-shot tokens, and
// the caller's own settings document (whose profile section /api/auth/me merges).
type authStore interface {
	storageif.UserStore
	storageif.UserTokenStore
	LoadUserSettings(ctx context.Context, userID string) (*storageif.AppSettings, error)
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

// SendVerificationEmail queues a new email-verification link for user on the mail runner, the token write
// included (queueTokenEmail); best-effort, never an error, and it waits on neither the store nor SMTP.
func (s *AuthService) SendVerificationEmail(ctx context.Context, user *storageif.User) {
	s.queueTokenEmail(ctx, user, storageif.TokenPurposeEmailVerify, EmailVerifyTTL, false,
		func(raw string) mailer.Mail {
			return mailer.EmailVerification.With(mailer.EmailVerificationData{Token: raw})
		})
}

// ResendVerification re-issues the verification link for a known, still-unverified account. Returns (nil, nil) —
// NOT an error — for unknown or already-verified addresses, so the caller responds identically in every case
// (anti-enumeration). The request only looks the account up: invalidating the outstanding tokens (so only the
// newest link redeems), writing the new one and sending it run on the mail runner.
func (s *AuthService) ResendVerification(ctx context.Context, email string) (*storageif.User, error) {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil || user == nil || user.EmailVerified {
		return nil, nil
	}
	s.queueTokenEmail(ctx, user, storageif.TokenPurposeEmailVerify, EmailVerifyTTL, true,
		func(raw string) mailer.Mail {
			return mailer.EmailVerification.With(mailer.EmailVerificationData{Token: raw})
		})
	return user, nil
}

// queueTokenEmail hands a one-time-token email to the mail runner as one job: invalidating user's outstanding
// tokens of purpose (when invalidate), writing the new token and sending its link (mail) all run there, so the
// request pays only the account lookup, which an unknown address pays too (B7). Without a mail service nothing is
// written: a token no email carries is of no use.
func (s *AuthService) queueTokenEmail(ctx context.Context, user *storageif.User, purpose string, ttl time.Duration, invalidate bool, mail func(raw string) mailer.Mail) {
	if s.email == nil || user.Email == "" {
		return
	}
	userID := user.ID
	err := s.email.EnqueueJob(ctx, user.Email, func(ctx context.Context) (mailer.Mail, error) {
		if invalidate {
			if err := s.backend.InvalidateUserTokens(ctx, userID, purpose); err != nil {
				logger.Error("failed to invalidate outstanding tokens before issuing a new one", "error", err, "userID", userID, "purpose", purpose)
			}
		}
		raw, hash, err := auth.GenerateOpaqueToken()
		if err != nil {
			return mailer.Mail{}, fmt.Errorf("%s token generation: %w", purpose, err)
		}
		tok := &storageif.UserToken{TokenHash: hash, Purpose: purpose, UserID: userID, ExpiresAt: time.Now().UTC().Add(ttl)}
		if err := s.backend.CreateUserToken(ctx, tok); err != nil {
			return mailer.Mail{}, fmt.Errorf("storing the %s token: %w", purpose, err)
		}
		return mail(raw), nil
	})
	if err != nil {
		logger.FromContext(ctx).Error("token email not queued", "error", err, "userID", userID, "purpose", purpose)
	}
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

// RequestPasswordReset queues a reset token and its email. Returns (nil, nil) — NOT an error — when email doesn't
// match a user, so the caller responds identically (anti-enumeration). The request only looks the account up: the
// token write and the SMTP round trip, which only an existing account pays, run on the mail runner (queueTokenEmail),
// so the response time no longer tells which accounts exist (B7).
func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) (*storageif.User, error) {
	user, err := s.backend.LoadUserByEmail(ctx, email)
	if err != nil || user == nil {
		return nil, nil
	}
	s.queueTokenEmail(ctx, user, storageif.TokenPurposePasswordReset, PasswordResetTTL, false,
		func(raw string) mailer.Mail {
			return mailer.PasswordReset.With(mailer.PasswordResetData{Token: raw})
		})
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

// User loads an account by ID; ErrNotFound when it does not exist.
func (s *AuthService) User(ctx context.Context, userID string) (*storageif.User, error) {
	return s.backend.LoadUserByID(ctx, userID)
}

// UpdateProfile sets the user's display name and avatar URL; an empty value clears it. servingURL is the avatar
// serving route (an API concern the caller passes in), and the stored image and the URL stay in step: an
// avatarURL equal to servingURL keeps the upload (and is refused with ErrAvatarImage when there is none, since
// it would point at a 404); any other value, an external URL or empty, replaces the upload, so the old image is
// deleted instead of staying served and exported behind a profile that no longer shows it.
func (s *AuthService) UpdateProfile(ctx context.Context, userID, displayName, avatarURL, servingURL string) error {
	if avatarURL == servingURL && avatarURL != "" {
		if _, err := s.backend.LoadUserAvatar(ctx, userID); err != nil {
			if errors.Is(err, storageif.ErrNotFound) {
				return fmt.Errorf("%w: there is no uploaded picture to point at", ErrAvatarImage)
			}
			return err
		}
		return s.backend.UpdateUserProfile(ctx, userID, displayName, avatarURL)
	}
	if err := s.backend.UpdateUserProfile(ctx, userID, displayName, avatarURL); err != nil {
		return err
	}
	return s.backend.DeleteUserAvatar(ctx, userID)
}

// ProfileFields returns the user's identity-card fields — their settings document's profile section. A user who
// never saved settings reads the defaults, whose profile section is empty.
func (s *AuthService) ProfileFields(ctx context.Context, userID string) (storageif.ProfileSettings, error) {
	settings, err := s.backend.LoadUserSettings(ctx, userID)
	if err != nil {
		return storageif.ProfileSettings{}, err
	}
	return settings.Profile, nil
}
