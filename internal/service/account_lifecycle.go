package service

import (
	"context"
	"io"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// AccountLifecycleService owns self-service account erasure and the data-subject export. Erasure runs in three
// phases: prepare reads what the SQL cascade is about to remove, delete erases the account, finalize releases the
// process state the cascade does not reach.
type AccountLifecycleService struct {
	users storageif.UserStore
	// providers releases the user's own Azure credential and disables the org credentials they lent as an admin;
	// nil = none (bare tests, desktop).
	providers *ProviderService
	// onUserDeleted runs after a successful erasure (wired to ProviderService.InvalidateScope): the SQL cascade
	// does not reach the in-process caches.
	onUserDeleted func(scope string)
}

// NewAccountLifecycleService builds the service over the user store; providers may be nil (bare tests, desktop).
func NewAccountLifecycleService(users storageif.UserStore, providers *ProviderService) *AccountLifecycleService {
	return &AccountLifecycleService{users: users, providers: providers}
}

// SetProviderService replaces the provider service; for tests that build a router before their provider service.
func (s *AccountLifecycleService) SetProviderService(p *ProviderService) { s.providers = p }

// OnUserDeleted registers the post-erasure hook; set once at boot.
func (s *AccountLifecycleService) OnUserDeleted(fn func(scope string)) { s.onUserDeleted = fn }

// DeleteAccount erases userID (GDPR). keyScope is the caller's credential scope. lentDisabled is called once per
// org credential finalize disabled, in order, so the caller can audit each. A failed delete returns its error
// before anything is released.
func (s *AccountLifecycleService) DeleteAccount(ctx context.Context, userID, keyScope string, lentDisabled func(storageif.OrgKeyRef)) error {
	release := s.prepare(keyScope)
	if err := s.users.DeleteUser(ctx, userID); err != nil {
		return err
	}
	s.finalize(ctx, userID, keyScope, release, lentDisabled)
	return nil
}

// prepare reads before the erasure cascades the provider keys away: the Azure document names the credential to
// release.
func (s *AccountLifecycleService) prepare(keyScope string) (release func()) {
	if s.providers == nil {
		return nil
	}
	return s.providers.PrepareAccountErasure(keyScope)
}

// finalize releases the user's own credential, disables every org credential they lent (org_provider_keys is
// org-keyed and does not cascade with the account), then runs the post-erasure hook. Best effort: a failure to
// disable is logged and what was disabled is still reported.
func (s *AccountLifecycleService) finalize(ctx context.Context, userID, keyScope string, release func(), lentDisabled func(storageif.OrgKeyRef)) {
	if release != nil {
		release()
	}
	if s.providers != nil && s.providers.OrgKeysAvailable() {
		refs, err := s.providers.DisableOrgAzureCredentialsLentBy(userID)
		if err != nil {
			logger.FromContext(ctx).Warn("account: disabling the user's shared org credentials failed", "error", err)
		}
		for _, ref := range refs {
			if lentDisabled != nil {
				lentDisabled(ref)
			}
		}
	}
	if s.onUserDeleted != nil {
		s.onUserDeleted(keyScope)
	}
}

// userDataStreamer is implemented by backends that can stream a data-subject export incrementally; backends
// without it use the buffered path.
type userDataStreamer interface {
	ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error
}

// StreamsExport reports whether the store can stream the export (StreamExport) instead of building it in memory
// (Export).
func (s *AccountLifecycleService) StreamsExport() bool {
	_, ok := s.users.(userDataStreamer)
	return ok
}

// StreamExport writes userID's data-subject export to w; ErrNotFound if the user does not exist. Only valid when
// StreamsExport.
func (s *AccountLifecycleService) StreamExport(ctx context.Context, userID string, w io.Writer) error {
	return s.users.(userDataStreamer).ExportUserDataTo(ctx, userID, w)
}

// Export assembles userID's data-subject export; ErrNotFound if the user does not exist.
func (s *AccountLifecycleService) Export(ctx context.Context, userID string) (*storageif.UserDataExport, error) {
	return s.users.ExportUserData(ctx, userID)
}
