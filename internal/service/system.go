package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/logger"
	"pad-core/models"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

// ErrSettingsInvalid refuses a settings write whose document breaks a field rule — today the profile section's
// length caps and its timezone's IANA name. The API maps it to 400.
var ErrSettingsInvalid = errors.New("invalid settings")

// The profile section's caps, mirrored by the frontend's constraints (frontend/src/lib/constraints.ts).
const (
	profileBioMaxChars      = 500
	profileJobTitleMaxChars = 100
)

// validateProfileSection checks the user-editable identity fields before a settings write stores them; the
// timezone must resolve like the analytics query's does (an unresolvable name would dead-render the dates that
// honour it).
func validateProfileSection(p models.ProfileSettings) error {
	if n := utf8.RuneCountInString(p.Bio); n > profileBioMaxChars {
		return fmt.Errorf("%w: profile.bio is %d characters, at most %d", ErrSettingsInvalid, n, profileBioMaxChars)
	}
	if n := utf8.RuneCountInString(p.JobTitle); n > profileJobTitleMaxChars {
		return fmt.Errorf("%w: profile.jobTitle is %d characters, at most %d", ErrSettingsInvalid, n, profileJobTitleMaxChars)
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("%w: profile.timezone %q is not an IANA zone name", ErrSettingsInvalid, p.Timezone)
		}
	}
	return nil
}

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

// SettingsSections is the set of top-level sections a settings write names (SettingsSectionsOf). A section the write
// leaves out keeps its stored value, so a client that predates a section, or one that sends only what it changed,
// cannot wipe the others; a named section is replaced, and an explicit {} or null clears it. nil names every
// section: the write is the whole document.
type SettingsSections map[string]bool

// serverOwnedSettingsKey is the one key of the document no settings write sets: the recent files are the server's
// list (AddRecentFile, RemoveRecentFile, ClearRecentFiles), and the web client echoes the document it loaded, so a
// write from a page loaded before a file was opened would take the list back.
const serverOwnedSettingsKey = "recentFiles"

// SettingsSectionsOf returns the sections a settings body names, each under the document's key for it (a body key
// matches as encoding/json matches it, regardless of case). A body that is not an object names none, and no body
// names the server-owned recent files.
func SettingsSectionsOf(body []byte) SettingsSections {
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	named := SettingsSections{}
	for _, key := range settingsdoc.DocumentKeys(models.AppSettings{}) {
		if key == serverOwnedSettingsKey {
			continue
		}
		for k := range top {
			if strings.EqualFold(k, key) {
				named[key] = true
			}
		}
	}
	return named
}

// keepUnnamed sets each section of settings the write does not name to stored's.
func (n SettingsSections) keepUnnamed(settings, stored *models.AppSettings) {
	if n == nil || stored == nil {
		return
	}
	dst, src := reflect.ValueOf(settings).Elem(), reflect.ValueOf(stored).Elem()
	for i := range dst.NumField() {
		key, _, _ := strings.Cut(dst.Type().Field(i).Tag.Get("json"), ",")
		if key != settingsdoc.DocVersionKey && !n[key] {
			dst.Field(i).Set(src.Field(i))
		}
	}
}

// UpdateUserSettings saves userID's own settings; an empty userID saves the deployment's (see GetUserSettings). The
// sections the write does not name keep their stored values (SettingsSections). The stored document is read before
// the write, not under its lock: two writers naming different sections at the same moment can still lose one's
// change, as two whole-document writes always could.
func (s *SystemService) UpdateUserSettings(ctx context.Context, userID string, settings models.AppSettings, sections SettingsSections) (err error) {
	defer logger.Guard("SystemService.UpdateUserSettings", &err)
	if err := validateProfileSection(settings.Profile); err != nil {
		return err
	}
	if s.backend == nil || userID == "" {
		if sections != nil {
			stored, err := s.GetSettings(ctx)
			if err != nil {
				return err
			}
			sections.keepUnnamed(&settings, stored)
		}
		return s.UpdateSettings(ctx, settings)
	}
	if sections != nil {
		stored, err := s.backend.LoadUserSettings(ctx, userID)
		if err != nil {
			return fmt.Errorf("load user settings: %w", err)
		}
		sections.keepUnnamed(&settings, settingsToModel(stored))
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
	// Sections: the top-level sections the write names; the others keep their stored values (SettingsSections).
	Sections SettingsSections
}

// UnknownProvidersError is ErrProviderUnknown for an allowed-provider list, naming every id no spec describes so the
// route can report them (details.providers) and the settings UI can uncheck them.
type UnknownProvidersError struct{ IDs []string }

func (e *UnknownProvidersError) Error() string {
	quoted := make([]string, len(e.IDs))
	for i, id := range e.IDs {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return fmt.Sprintf("%v: %s in allowedProviders", ErrProviderUnknown, strings.Join(quoted, ", "))
}

func (e *UnknownProvidersError) Unwrap() error { return ErrProviderUnknown }

// UpdateOrgSettings replaces the sections of the org's settings document the write names (OrgSettingsWrite),
// keeping the stored allowed-provider list unless the write names it. A named list is validated: ids the registry
// does not know are an *UnknownProvidersError, which is ErrProviderUnknown (the list is fail-closed, so a typo would
// lock the org out of every provider).
func (s *SystemService) UpdateOrgSettings(ctx context.Context, orgID string, settings models.AppSettings, write OrgSettingsWrite) (err error) {
	defer logger.Guard("SystemService.UpdateOrgSettings", &err)
	if write.AllowedProvidersSet {
		var unknown []string
		for _, id := range settings.AI.AllowedProviders {
			if !ai.KnownProvider(id) {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			return &UnknownProvidersError{IDs: unknown}
		}
	}
	if s.backend == nil {
		if write.Sections != nil {
			stored, err := s.GetSettings(ctx)
			if err != nil {
				return err
			}
			write.Sections.keepUnnamed(&settings, stored)
		}
		return s.UpdateSettings(ctx, settings)
	}
	if write.Sections != nil || !write.AllowedProvidersSet {
		stored, err := s.backend.LoadOrgSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if stored != nil {
			write.Sections.keepUnnamed(&settings, settingsToModel(stored))
		}
		if !write.AllowedProvidersSet {
			settings.AI.AllowedProviders = nil
			if stored != nil {
				settings.AI.AllowedProviders = stored.AI.AllowedProviders
			}
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
