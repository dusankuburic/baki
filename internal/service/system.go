package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

// SystemService handles settings, info, error logging, and API keys.
type SystemService struct {
	settings SettingsProvider
	secrets  KeyStore
	notifier EventNotifier
	backend  storageif.StorageBackend
	mode     config.DeploymentMode
	// bus carries CredentialChanged (the provider model cache drops the listing) and SettingsChanged (the
	// settings cache and the rule profiles drop); nil publishes nothing.
	bus *eventbus.Bus
	// azure is the Azure AI Foundry model registry (Part 9): with it, "has a key"
	// means "has a resource", and deleting the azure-foundry key removes resource
	// "default" with its models. nil = the single document.
	azure *AzureModelService
}

// NewSystemService builds the system service; azure is the Azure AI Foundry model registry /api/keys acts on for
// azure-foundry (nil keeps the single document). bus announces credential and settings changes (nil: none).
func NewSystemService(settings SettingsProvider, secrets KeyStore, notifier EventNotifier, backend storageif.StorageBackend, mode config.DeploymentMode, azure *AzureModelService, bus *eventbus.Bus) *SystemService {
	return &SystemService{settings: settings, secrets: secrets, notifier: notifier, backend: backend, mode: mode, azure: azure, bus: bus}
}

// B5: the settings methods thread the caller's context.Context through to the storage
// backend so a client disconnect / cancellation propagates to the DB call (they used
// to drop the request context via context.Background(), keeping requests alive past
// client cancellation). The in-memory fallbacks don't do I/O, so ctx is unused there.

// GetSettings returns the deployment-wide settings. It used to branch on s.backend
// and read Postgres directly — a SECOND source of truth next to the SettingsProvider
// (in cloud mode a local file nothing durable wrote); both now go through the
// provider (backend-backed in cloud mode, file-backed on the desktop), one path only.
func (s *SystemService) GetSettings(_ context.Context) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetSettings", &err)
	if s.settings == nil {
		return models.DefaultSettings(), nil
	}
	return s.settings.Get(), nil
}

func (s *SystemService) UpdateSettings(ctx context.Context, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateSettings", &err)
	if s.settings == nil {
		return fmt.Errorf("settings store not initialized")
	}
	// ctx carries the request's RLS transaction in cloud mode, so a request
	// that writes settings and then fails has the write rolled back with it.
	if err = s.settings.Update(ctx, settings); err != nil {
		return fmt.Errorf("persist settings: %w", err)
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	// Reached only without a server store (the desktop): its clients are the local user's.
	s.notifier.EmitTo(LocalUser, "settings:changed", settings)
	return nil
}

// GetUserSettings returns userID's own settings from the server store. An empty userID is the one local user of a
// deployment without accounts (the caller's key scope): their settings are the deployment's, as on the desktop, since
// a database has no users row to keep them under.
func (s *SystemService) GetUserSettings(ctx context.Context, userID string) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetUserSettings", &err)
	if s.backend == nil || userID == "" {
		return s.GetSettings(ctx)
	}
	ifaceSettings, err := s.backend.LoadUserSettings(ctx, userID)
	if err != nil {
		return nil, err
	}
	return settingsToModel(ifaceSettings), nil
}

// UpdateUserSettings saves userID's own settings; an empty userID saves the deployment's (see GetUserSettings).
func (s *SystemService) UpdateUserSettings(ctx context.Context, userID string, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateUserSettings", &err)
	if s.backend == nil || userID == "" {
		return s.UpdateSettings(ctx, settings)
	}
	if err := s.backend.SaveUserSettings(ctx, userID, settingsFromModel(&settings)); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.UserSettings, ID: userID})
	s.notifier.EmitTo(userID, "settings:changed", settings)
	return nil
}

func (s *SystemService) GetOrgSettings(ctx context.Context, orgID string) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetOrgSettings", &err)
	if s.backend == nil {
		return s.GetSettings(ctx)
	}
	ifaceSettings, err := s.backend.LoadOrgSettings(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return settingsToModel(ifaceSettings), nil
}

// OrgSettingsWrite says which parts of an org settings write the client meant: the wire cannot tell an absent
// ai.allowedProviders from an empty one (omitempty), so the handler reports whether the body named it.
type OrgSettingsWrite struct {
	// AllowedProvidersSet: the write names ai.allowedProviders (an empty list clears it). Unset, the stored list
	// is kept — a partial write (the Rules panel posts {analysis:{rules}}) must not wipe the org's policy.
	AllowedProvidersSet bool
}

// UpdateOrgSettings replaces the org's settings document with settings, keeping the stored allowed-provider
// list unless the write names it (OrgSettingsWrite). A named list is validated: an id the registry does not
// know is ErrProviderUnknown (the list is fail-closed, so a typo would lock the org out of every provider).
func (s *SystemService) UpdateOrgSettings(ctx context.Context, orgID string, settings models.AppSettings, write OrgSettingsWrite) (err error) {
	defer logger.Guard("SystemService.UpdateOrgSettings", &err)
	if write.AllowedProvidersSet {
		for _, id := range settings.AI.AllowedProviders {
			if !ai.KnownProvider(id) {
				return fmt.Errorf("%w: %q in allowedProviders", ErrProviderUnknown, id)
			}
		}
	}
	if s.backend == nil {
		return s.UpdateSettings(ctx, settings)
	}
	if !write.AllowedProvidersSet {
		stored, err := s.backend.LoadOrgSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		settings.AI.AllowedProviders = nil
		if stored != nil {
			settings.AI.AllowedProviders = stored.AI.AllowedProviders
		}
	}
	if err := s.backend.SaveOrgSettings(ctx, orgID, settingsFromModel(&settings)); err != nil {
		return err
	}
	// The org's settings are its rule profile (Analysis.Rules).
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: orgID})
	// The members (owner included) are resolved where the event is delivered, on ctx. Best-effort: a roster that
	// cannot be read is logged there and never fails the persisted update.
	s.notifier.EmitToOrg(ctx, orgID, "settings:changed", settings)
	return nil
}

// AppInfo returns the deployment's version/platform/build metadata.
func (s *SystemService) AppInfo() (info *models.AppInfo, err error) {
	defer logger.Guard("SystemService.AppInfo", &err)
	return &models.AppInfo{
		Version:   Version,
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		BuildDate: BuildDate,
		GitCommit: GitCommit,
		Capabilities: models.AppCapabilities{
			SessionAnalytics: s.mode == config.ModeLocal,
		},
	}, nil
}

// maxFrontendErrorField caps each field of a client error report in the log; the HTTP
// layer bounds the whole body (64 KiB) — this keeps one field from being the whole
// budget (a log line is read by people, not stored blobs).
const maxFrontendErrorField = 8 << 10

// LogError records a client-side error report through errreport, not only the log:
// that package is the one place exceptions are aggregated (it counts
// pad_errors_reported_total and forwards to a registered sink), and browser errors —
// the largest single source — used to bypass it, invisible to counter and triage.
func (s *SystemService) LogError(ctx context.Context, payload models.FrontendError) {
	message := truncateForLog(payload.Message, maxFrontendErrorField)
	errreport.CaptureError(ctx, "frontend", errors.New(message), errreport.Attrs{
		"stack":          truncateForLog(payload.Stack, maxFrontendErrorField),
		"componentStack": truncateForLog(payload.ComponentStack, maxFrontendErrorField),
		"url":            truncateForLog(payload.URL, 2048),
	})
}

// truncateForLog cuts s to max bytes with a visible marker, on a rune boundary.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[truncated %d bytes]", len(s)-cut)
}

// SaveApiKey stores a credential and announces it: the provider's cached live listing must not outlive the key
// it was listed with (a replacement key may carry other entitlements), nor may a listing in flight write it back.
func (s *SystemService) SaveApiKey(ctx context.Context, scope, provider string, key string) (err error) {
	defer logger.Guard("SystemService.SaveApiKey", &err)
	if err := s.secrets.Save(scope, provider, key); err != nil {
		return err
	}
	s.credentialChanged(ctx, scope, provider, events.CredentialSaved)
	return nil
}

func (s *SystemService) credentialChanged(ctx context.Context, scope, provider string, kind events.CredentialChangeKind) {
	s.bus.Publish(ctx, events.CredentialChanged{Scope: scope, ProviderID: provider, Kind: kind})
}

// listingHook is provider's family hook: the registry this service was built with, else the one installed on
// the spec (DI), else nil — the keystore row answers.
func (s *SystemService) listingHook(provider string) ai.ListingHook {
	if registryFamily(s.azure, provider) {
		return azureListing{registry: s.azure}
	}
	if spec, ok := ai.SpecOf(provider); ok {
		return spec.Listing
	}
	return nil
}

func (s *SystemService) HasApiKey(scope, provider string) (result bool, err error) {
	defer logger.Guard("SystemService.HasApiKey", &err)
	if hook := s.listingHook(provider); hook != nil {
		// A family (several Azure resources): any resource of the caller's own.
		return hook.Configured(context.Background(), scope)
	}
	return s.secrets.Has(scope, provider)
}

// DeleteApiKey removes a credential (for azure-foundry with the registry, resource "default" with its models) and
// announces it like SaveApiKey.
func (s *SystemService) DeleteApiKey(ctx context.Context, scope, provider string) (err error) {
	defer logger.Guard("SystemService.DeleteApiKey", &err)
	if l, ok := s.listingHook(provider).(azureListing); ok {
		// Deprecated for the registry (one release): the key is resource
		// "default"'s credential — the resource goes with its models, and
		// its credential after the commit.
		if err := l.registry.legacyDeleteDefault(context.Background(), l.registry.userOwner(scope), scope); err != nil {
			return err
		}
		s.credentialChanged(ctx, scope, provider, events.CredentialDeleted)
		return nil
	}
	// The Azure AI Foundry document registers process-wide state beyond the
	// KeyStore (its Entra ID token source and cached listing); read it before
	// the row is gone so it can be released with it.
	var azureDoc string
	if provider == ai.AzureFoundryProviderID {
		azureDoc, _ = s.secrets.Get(scope, provider)
	}
	if err := s.secrets.Delete(scope, provider); err != nil {
		return err
	}
	if azureDoc != "" {
		if cfg, perr := ai.ParseAzureFoundryConfig(azureDoc); perr == nil {
			ai.ForgetAzureFoundryCredential(cfg)
		}
	}
	s.credentialChanged(ctx, scope, provider, events.CredentialDeleted)
	return nil
}
