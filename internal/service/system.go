package service

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"

	"pad-analyzer/internal/config"
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
	secrets  SecretStore
	notifier Notifier
	backend  storageif.StorageBackend
	mode     config.DeploymentMode
}

func NewSystemService(settings SettingsProvider, secrets SecretStore, notifier Notifier, backend storageif.StorageBackend, mode config.DeploymentMode) *SystemService {
	return &SystemService{settings: settings, secrets: secrets, notifier: notifier, backend: backend, mode: mode}
}

func (s *SystemService) GetSettings() (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetSettings", &err)
	if s.backend != nil {
		ifaceSettings, err := s.backend.LoadSettings(context.Background())
		if err != nil {
			return nil, fmt.Errorf("load settings: %w", err)
		}
		return s.toModel(ifaceSettings), nil
	}
	if s.settings == nil {
		return models.DefaultSettings(), nil
	}
	return s.settings.Get(), nil
}

func (s *SystemService) UpdateSettings(settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateSettings", &err)
	if s.backend != nil {
		if err := s.backend.SaveSettings(context.Background(), s.fromModel(&settings)); err != nil {
			return fmt.Errorf("persist settings: %w", err)
		}
		s.notifier.Emit("settings:changed", settings)
		return nil
	}
	// Desktop/local mode — no backend, use in-memory store
	if s.settings == nil {
		return fmt.Errorf("settings store not initialized")
	}
	if err = s.settings.Update(settings); err != nil {
		return err
	}
	s.notifier.Emit("settings:changed", settings)
	return nil
}

func (s *SystemService) GetUserSettings(userID string) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetUserSettings", &err)
	if s.backend == nil {
		return s.GetSettings()
	}
	ifaceSettings, err := s.backend.LoadUserSettings(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	return s.toModel(ifaceSettings), nil
}

func (s *SystemService) UpdateUserSettings(userID string, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateUserSettings", &err)
	if s.backend == nil {
		return s.UpdateSettings(settings)
	}
	if err := s.backend.SaveUserSettings(context.Background(), userID, s.fromModel(&settings)); err != nil {
		return err
	}
	s.notifier.EmitTo(userID, "settings:changed", settings)
	return nil
}

func (s *SystemService) GetOrgSettings(orgID string) (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetOrgSettings", &err)
	if s.backend == nil {
		return s.GetSettings()
	}
	ifaceSettings, err := s.backend.LoadOrgSettings(context.Background(), orgID)
	if err != nil {
		return nil, err
	}
	return s.toModel(ifaceSettings), nil
}

func (s *SystemService) UpdateOrgSettings(orgID string, settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateOrgSettings", &err)
	if s.backend == nil {
		return s.UpdateSettings(settings)
	}
	if err := s.backend.SaveOrgSettings(context.Background(), orgID, s.fromModel(&settings)); err != nil {
		return err
	}
	s.notifier.Emit("settings:changed", settings)
	return nil
}

// toModel/fromModel bridge the storage-layer and domain settings structs, which
// are intentionally kept JSON-compatible, via a marshal round-trip. The errors
// effectively never fire for these mirror structs, but they're logged rather
// than discarded so a future field-type mismatch surfaces instead of silently
// dropping data. TestSettingsRoundTrip guards against the structs drifting apart.
func (s *SystemService) toModel(is *storageif.AppSettings) *models.AppSettings {
	if is == nil {
		return models.DefaultSettings()
	}
	data, err := json.Marshal(is)
	if err != nil {
		logger.Warn("SystemService.toModel: marshal storage settings", "error", err)
		return models.DefaultSettings()
	}
	var m models.AppSettings
	if err := json.Unmarshal(data, &m); err != nil {
		logger.Warn("SystemService.toModel: unmarshal into model settings", "error", err)
		return models.DefaultSettings()
	}
	return &m
}

func (s *SystemService) fromModel(m *models.AppSettings) *storageif.AppSettings {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		logger.Warn("SystemService.fromModel: marshal model settings", "error", err)
		return nil
	}
	var is storageif.AppSettings
	if err := json.Unmarshal(data, &is); err != nil {
		logger.Warn("SystemService.fromModel: unmarshal into storage settings", "error", err)
		return nil
	}
	return &is
}

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

func (s *SystemService) LogError(payload models.FrontendError) {
	logger.Error("frontend error",
		"message", payload.Message,
		"stack", payload.Stack,
		"componentStack", payload.ComponentStack,
		"url", payload.URL,
	)
}

func (s *SystemService) SaveApiKey(scope, provider string, key string) (err error) {
	defer logger.Guard("SystemService.SaveApiKey", &err)
	return s.secrets.Save(scope, provider, key)
}

func (s *SystemService) HasApiKey(scope, provider string) (result bool, err error) {
	defer logger.Guard("SystemService.HasApiKey", &err)
	return s.secrets.Has(scope, provider)
}

func (s *SystemService) DeleteApiKey(scope, provider string) (err error) {
	defer logger.Guard("SystemService.DeleteApiKey", &err)
	return s.secrets.Delete(scope, provider)
}
