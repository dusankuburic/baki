package service

import (
	"fmt"
	"runtime"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

// SystemService handles settings, info, error logging, and API keys.
type SystemService struct {
	settings *storage.SettingsStore
	notifier Notifier
}

func NewSystemService(settings *storage.SettingsStore, notifier Notifier) *SystemService {
	return &SystemService{settings: settings, notifier: notifier}
}

func (s *SystemService) GetSettings() (settings *models.AppSettings, err error) {
	defer logger.Guard("SystemService.GetSettings", &err)
	if s.settings == nil {
		return models.DefaultSettings(), nil
	}
	return s.settings.Get(), nil
}

func (s *SystemService) UpdateSettings(settings models.AppSettings) (err error) {
	defer logger.Guard("SystemService.UpdateSettings", &err)
	if s.settings == nil {
		return fmt.Errorf("settings store not initialized")
	}
	if err = s.settings.Update(settings); err != nil {
		return err
	}
	s.notifier.Emit("settings:changed", settings)
	return nil
}

func (s *SystemService) AppInfo() (info *models.AppInfo, err error) {
	defer logger.Guard("SystemService.AppInfo", &err)
	return &models.AppInfo{
		Version:   Version,
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		BuildDate: BuildDate,
		GitCommit: GitCommit,
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
	return storage.SaveApiKeyScoped(scope, provider, key)
}

func (s *SystemService) HasApiKey(scope, provider string) (result bool, err error) {
	defer logger.Guard("SystemService.HasApiKey", &err)
	return storage.HasApiKeyScoped(scope, provider)
}

func (s *SystemService) DeleteApiKey(scope, provider string) (err error) {
	defer logger.Guard("SystemService.DeleteApiKey", &err)
	return storage.DeleteApiKeyScoped(scope, provider)
}
