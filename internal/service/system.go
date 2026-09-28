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
	// onKeyChanged runs after SaveApiKey stored or DeleteApiKey removed a
	// credential (wired in di/services.go to
	// ProviderService.InvalidateModels); nil = no hook.
	onKeyChanged func(scope, provider string)
}

// OnApiKeyChanged registers the hook SaveApiKey and DeleteApiKey run after
// a successful write. Post-construction wiring (same pattern as the
// dashboard and chat cache hooks): the provider service's cached live
// listing for that provider — a removed Azure AI Foundry configuration's
// deployments, or the entitlements of a key that was replaced by another —
// must not outlive the credential, and a listing in flight must not write
// it back. Set once at boot, before requests are served.
func (s *SystemService) OnApiKeyChanged(fn func(scope, provider string)) {
	s.onKeyChanged = fn
}

func NewSystemService(settings SettingsProvider, secrets KeyStore, notifier EventNotifier, backend storageif.StorageBackend, mode config.DeploymentMode) *SystemService {
	return &SystemService{settings: settings, secrets: secrets, notifier: notifier, backend: backend, mode: mode}
}

// B5: the settings methods thread the caller's context.Context through to the
// storage backend so a client disconnect / cancellation propagates to the DB
// call (previously these dropped the request context via context.Background(),
// keeping requests alive past client cancellation). The in-memory fallbacks
// (SettingsProvider, secrets) don't do I/O so ctx is passed but unused there.

// GetSettings returns the deployment-wide settings.
//
// This used to branch on s.backend and read Postgres directly, which made it a
// SECOND source of truth: every other reader of deployment settings (the
// analysis rule profile, the AI daily budget, the embedding provider) went
// through the SettingsProvider, which in cloud mode was a local file that
// nothing durable ever wrote to. Both now go through the provider, which is
// backend-backed in cloud mode (BackendSettingsProvider) and file-backed on the
// desktop, so there is exactly one path to these values.
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
	s.notifier.Emit("settings:changed", settings)
	return nil
}

func (s *SystemService) GetUserSettings(ctx context.Context, userID string) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetUserSettings", &err)
	if s.backend == nil {
		return s.GetSettings(ctx)
	}
	ifaceSettings, err := s.backend.LoadUserSettings(ctx, userID)
	if err != nil {
		return nil, err
	}
	return settingsToModel(ifaceSettings), nil
}

func (s *SystemService) UpdateUserSettings(ctx context.Context, userID string, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateUserSettings", &err)
	if s.backend == nil {
		return s.UpdateSettings(ctx, settings)
	}
	if err := s.backend.SaveUserSettings(ctx, userID, settingsFromModel(&settings)); err != nil {
		return err
	}
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

func (s *SystemService) UpdateOrgSettings(ctx context.Context, orgID string, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateOrgSettings", &err)
	if s.backend == nil {
		return s.UpdateSettings(ctx, settings)
	}
	if err := s.backend.SaveOrgSettings(ctx, orgID, settingsFromModel(&settings)); err != nil {
		return err
	}
	s.emitOrgSettingsChanged(ctx, orgID, settings)
	return nil
}

// emitOrgSettingsChanged notifies one org's members that their analysis
// configuration changed.
//
// This used to be a bare Emit, which fans out to EVERY connected SSE client in
// the deployment — so one org's rule profile (which rules it has disabled, at
// what severity, with what thresholds) was delivered to every other tenant's
// users. EmitTo is the documented requirement for anything user-scoped, and
// UpdateUserSettings above already honours it; org settings are scoped too,
// just to a set of users rather than one.
//
// The EventManager indexes clients by user, not by org, so the fan-out happens
// here: load the org, address each member. That costs one storage read on a
// write path an admin triggers by hand, which is the right trade for not
// having to maintain an org→client index that would have to stay correct
// across membership changes and reconnects.
//
// Best-effort by design: the settings are already persisted when this runs, so
// a failure to notify must not fail the update. It is logged, never silent.
func (s *SystemService) emitOrgSettingsChanged(ctx context.Context, orgID string, settings models.AppSettings) {
	org, err := s.backend.LoadOrg(ctx, orgID)
	if err != nil || org == nil {
		logger.Warn("org settings saved but members could not be notified",
			"orgId", orgID, "error", err)
		return
	}
	// The owner is not necessarily in Members, so address them explicitly and
	// dedupe — a double EmitTo would deliver the event twice to one client.
	seen := make(map[string]bool, len(org.Members)+1)
	for _, id := range append([]string{org.OwnerID}, memberIDs(org.Members)...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		s.notifier.EmitTo(id, "settings:changed", settings)
	}
}

func memberIDs(members []storageif.OrgMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.UserID)
	}
	return out
}

// toModel/fromModel bridge the storage-layer and domain settings structs, which
// are intentionally kept JSON-compatible, via a marshal round-trip. The errors
// effectively never fire for these mirror structs, but they're logged rather
// than discarded so a future field-type mismatch surfaces instead of silently
// dropping data. TestSettingsRoundTrip guards against the structs drifting apart.
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

// maxFrontendErrorField caps each field of a client error report in the log.
// The HTTP layer bounds the whole body (64 KiB); this keeps one field from
// being the whole budget — a log line is read by people, not stored blobs.
const maxFrontendErrorField = 8 << 10

// LogError records a client-side error report. It goes through errreport, not
// only the log: that package is meant to be the one place exceptions are
// aggregated (it counts pad_errors_reported_total and forwards to a registered
// sink), and browser errors — the largest single source of them — bypassed it
// entirely, so they were invisible to both the counter and any crash-triage
// backend.
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

func (s *SystemService) SaveApiKey(scope, provider string, key string) (err error) {
	defer logger.Guard("SystemService.SaveApiKey", &err)
	if err := s.secrets.Save(scope, provider, key); err != nil {
		return err
	}
	if s.onKeyChanged != nil {
		s.onKeyChanged(scope, provider)
	}
	return nil
}

func (s *SystemService) HasApiKey(scope, provider string) (result bool, err error) {
	defer logger.Guard("SystemService.HasApiKey", &err)
	return s.secrets.Has(scope, provider)
}

func (s *SystemService) DeleteApiKey(scope, provider string) (err error) {
	defer logger.Guard("SystemService.DeleteApiKey", &err)
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
	if s.onKeyChanged != nil {
		s.onKeyChanged(scope, provider)
	}
	return nil
}
