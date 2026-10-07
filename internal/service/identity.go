package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// IdentityLinkStore is the optional storage capability for SSO identity links, implemented by the Postgres
// backend.
type IdentityLinkStore interface {
	SaveIdentityLink(ctx context.Context, link *storageif.IdentityLink) error
	LoadIdentityLink(ctx context.Context, provider, subject string) (*storageif.IdentityLink, error)
	ListIdentityLinks(ctx context.Context, userID string) ([]*storageif.IdentityLink, error)
	DeleteIdentityLink(ctx context.Context, userID, provider string) error
}

// ErrUnlinkOnlySignIn refuses removing the link of an account without a password: the link is then its only way
// to sign in.
var ErrUnlinkOnlySignIn = errors.New("set a password first — this identity is currently the only way to sign in")

// IdentityService maps external (SSO) identities to local accounts: first-login provisioning, account linking
// and unlinking. Without a link store (no OIDC-capable backend) Available is false.
type IdentityService struct {
	users authStore
	links IdentityLinkStore
}

func NewIdentityService(users authStore, links IdentityLinkStore) *IdentityService {
	return &IdentityService{users: users, links: links}
}

// Available reports whether identity links can be stored; safe on a nil service.
func (s *IdentityService) Available() bool { return s != nil && s.links != nil }

// Link attaches ident to userID: an identity already linked to a different account is refused (one external
// identity maps to exactly one local user); re-linking the same one is a no-op.
func (s *IdentityService) Link(ctx context.Context, provider, userID string, ident *sso.Identity) error {
	if existing, err := s.links.LoadIdentityLink(ctx, provider, ident.Subject); err == nil {
		if existing.UserID == userID {
			return nil
		}
		return fmt.Errorf("this %s identity is already linked to another account", provider)
	} else if !errors.Is(err, storageif.ErrNotFound) {
		return err
	}
	return s.links.SaveIdentityLink(ctx, &storageif.IdentityLink{
		Provider: provider,
		Subject:  ident.Subject,
		UserID:   userID,
		Email:    strings.ToLower(ident.Email),
	})
}

// Links lists userID's linked identities.
func (s *IdentityService) Links(ctx context.Context, userID string) ([]*storageif.IdentityLink, error) {
	return s.links.ListIdentityLinks(ctx, userID)
}

// Unlink removes userID's link for provider; ErrUnlinkOnlySignIn when the account has no password.
func (s *IdentityService) Unlink(ctx context.Context, userID, provider string) error {
	user, err := s.users.LoadUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.Password == "" {
		return ErrUnlinkOnlySignIn
	}
	return s.links.DeleteIdentityLink(ctx, userID, provider)
}

// ResolveSSOUser maps a verified IdP identity to a local user: an existing link wins; else an account with the
// same verified email is linked; else a new account is JIT-provisioned with no usable password. Linking and
// provisioning both require the IdP to assert the email as verified — auto-linking on an unverified email would
// let an attacker who controls a sloppy IdP account take over the matching local account.
func (s *IdentityService) ResolveSSOUser(ctx context.Context, provider string, ident *sso.Identity) (*storageif.User, error) {
	ident.Email = strings.ToLower(ident.Email)

	if link, err := s.links.LoadIdentityLink(ctx, provider, ident.Subject); err == nil {
		return s.users.LoadUserByID(ctx, link.UserID)
	} else if !errors.Is(err, storageif.ErrNotFound) {
		return nil, fmt.Errorf("login failed — try again")
	}

	if ident.Email == "" || !ident.EmailVerified {
		return nil, fmt.Errorf("your identity provider did not assert a verified email address")
	}

	newLink := &storageif.IdentityLink{
		Provider: provider,
		Subject:  ident.Subject,
		Email:    ident.Email,
	}

	if existing, err := s.users.LoadUserByEmail(ctx, ident.Email); err == nil {
		newLink.UserID = existing.ID

		// Security: an unverified local account (e.g. an open-registration "shadow" with a password an attacker
		// knows) is a pre-account-takeover risk. Linking it to a verified SSO identity takes ownership: clear the
		// password hash so the shadow's password can no longer log in, forcing the account onto the IdP.
		if !existing.EmailVerified {
			existing.EmailVerified = true
			existing.Password = "" // Strip the insecure password hash
			if err := s.users.SaveUser(ctx, existing); err != nil {
				return nil, fmt.Errorf("failed to harden account — try again")
			}
			// Outstanding reset/verify tokens must go too: an attacker who created the shadow could have a reset
			// link that would re-arm a known password after the owner takes over via SSO.
			if err := s.users.InvalidateUserTokens(ctx, existing.ID,
				storageif.TokenPurposePasswordReset, storageif.TokenPurposeEmailVerify); err != nil {
				logger.FromContext(ctx).Error("failed to invalidate outstanding reset/verify tokens during SSO account hardening", "error", err, "userID", existing.ID)
			}
			logger.Info("account takeover prevented: cleared password on unverified account linked to verified SSO identity", "userID", existing.ID, "email", audit.RedactEmail(ident.Email))
		} else {
			// Already verified: do NOT auto-link a new IdP identity — an attacker controlling an IdP that asserts
			// the victim's email as verified could SSO straight into the account. The owner must add the provider
			// from account settings.
			return nil, fmt.Errorf("an account with this email already exists — link %s from your account settings", provider)
		}

		if err := s.links.SaveIdentityLink(ctx, newLink); err != nil {
			return nil, fmt.Errorf("failed to link account — try again")
		}
		return existing, nil
	}

	user := &storageif.User{
		ID:    uuid.NewString(),
		Email: ident.Email,
		// No usable password: SSO-only account. bcrypt comparison against an empty hash always fails, so
		// password login is naturally impossible.
		Password:      "",
		EmailVerified: true, // IdP asserted verification
		Role:          auth.RoleMember,
		DisplayName:   ident.DisplayName,
	}
	if err := s.users.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("failed to create account — try again")
	}
	newLink.UserID = user.ID
	if err := s.links.SaveIdentityLink(ctx, newLink); err != nil {
		return nil, fmt.Errorf("failed to link account — try again")
	}
	return user, nil
}
