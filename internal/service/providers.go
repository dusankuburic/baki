package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"
)

// ProviderService manages AI provider configuration, authentication, and connectivity.
type ProviderService struct {
	ctx         context.Context
	auth        *ai.GitHubAuth
	copilotAuth *ai.CopilotAuth
	factory     *ai.ProviderFactory
}

func NewProviderService(ctx context.Context, auth *ai.GitHubAuth, copilotAuth *ai.CopilotAuth, factory *ai.ProviderFactory) *ProviderService {
	return &ProviderService{ctx: ctx, auth: auth, copilotAuth: copilotAuth, factory: factory}
}

func (s *ProviderService) ListProviders() (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	for _, meta := range ai.AvailableProviders() {
		p := ai.GetMetadataProvider(meta.ID)
		if p == nil {
			continue
		}

		configured := false
		switch meta.ID {
		case "github-models":
			ok, _ := storage.HasApiKey("github-models-token")
			configured = ok
		case "copilot":
			oauthOk, _ := storage.HasApiKey("copilot-oauth-token")
			patOk, _ := storage.HasApiKey("copilot")
			configured = oauthOk || patOk
		default:
			ok, _ := storage.HasApiKey(meta.ID)
			configured = ok
		}

		modelInfos := p.Models()
		modelDetails := make([]models.ModelDetail, len(modelInfos))
		for i, m := range modelInfos {
			modelDetails[i] = models.ModelDetail{
				ID:             m.ID,
				DisplayName:    m.DisplayName,
				ContextLimit:   m.ContextLimit,
				InputCostPerM:  m.Pricing.InputCostPerM,
				OutputCostPerM: m.Pricing.OutputCostPerM,
			}
		}

		providers = append(providers, models.ProviderInfo{
			ID:           meta.ID,
			Name:         meta.Name,
			Configured:   configured,
			Models:       modelDetails,
			DefaultModel: p.DefaultModel(),
			ContextLimit: p.ContextLimit(),
			AuthType:     meta.AuthType,
		})
	}

	if ai.DemoProxyURL != "" {
		providers = append(providers, models.ProviderInfo{
			ID:         "demo",
			Name:       "Demo",
			Configured: true,
			Models: []models.ModelDetail{{
				ID:           "demo",
				DisplayName:  "Demo",
				ContextLimit: 200000,
			}},
			DefaultModel: "demo",
			ContextLimit: 200000,
			AuthType:     "none",
		})
	}

	return providers, nil
}

func (s *ProviderService) TestProviderConnection(providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestProviderConnection", &err)

	provider, err := s.factory.For(providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: err.Error()}, nil
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()

	testModels := []string{provider.DefaultModel()}
	if fm := provider.FreeModel(); fm != "" && fm != provider.DefaultModel() {
		testModels = append(testModels, fm)
	}

	var chatErr error
	for _, model := range testModels {
		chatErr = nil
		_, chatErr = provider.Chat(ctx, ai.Request{
			Model:       model,
			Messages:    []ai.Message{{Role: "user", Content: "Hi"}},
			MaxTokens:   5,
			Temperature: 0,
		})
		if chatErr == nil {
			break
		}
		if errors.Is(chatErr, ai.ErrApiKeyInvalid) {
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: "invalid API key"}, nil
		}
		if errors.Is(chatErr, ai.ErrRateLimited) {
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: true, Latency: latency}, nil
		}
		if errors.Is(chatErr, ai.ErrInsufficientBalance) {
			continue
		}
		break
	}

	latency := int(time.Since(start).Milliseconds())

	if chatErr != nil {
		return &models.ProviderTestResult{Ok: false, Latency: latency, Error: chatErr.Error()}, nil
	}

	return &models.ProviderTestResult{Ok: true, Latency: latency}, nil
}

// --- GitHub Models OAuth ---

func (s *ProviderService) StartGitHubAuth() (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartGitHubAuth", &err)
	return s.auth.StartDeviceFlow(s.ctx)
}

func (s *ProviderService) PollGitHubAuth(deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollGitHubAuth", &err)

	result, err = s.auth.PollToken(s.ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := storage.SaveApiKey("github-models-token", result.Token); saveErr != nil {
			logger.Error("failed to save github token", "error", saveErr)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeGitHubAuth() (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	return storage.DeleteApiKey("github-models-token")
}

func (s *ProviderService) GetGitHubUser() (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)

	token, err := storage.GetApiKey("github-models-token")
	if err != nil {
		return nil, fmt.Errorf("no github token: %w", err)
	}
	return s.auth.GetUser(s.ctx, token)
}

// --- GitHub Copilot OAuth ---

func (s *ProviderService) StartCopilotAuth() (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartCopilotAuth", &err)
	return s.copilotAuth.StartDeviceFlow(s.ctx)
}

func (s *ProviderService) PollCopilotAuth(deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollCopilotAuth", &err)

	result, err = s.copilotAuth.PollToken(s.ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := storage.SaveApiKey("copilot-oauth-token", result.Token); saveErr != nil {
			logger.Error("failed to save copilot oauth token", "error", saveErr)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeCopilotAuth() (err error) {
	defer logger.Guard("App.RevokeCopilotAuth", &err)
	return storage.DeleteApiKey("copilot-oauth-token")
}

func (s *ProviderService) GetCopilotUser() (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)

	token, err := storage.GetApiKey("copilot-oauth-token")
	if err != nil {
		return nil, fmt.Errorf("no copilot oauth token: %w", err)
	}
	return s.auth.GetUser(s.ctx, token)
}
