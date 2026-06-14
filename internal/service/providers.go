package service

import (
	"context"
	"errors"
	"time"

	"pad-analyzer/internal/ai"
	"pad-core/logger"
	"pad-core/models"
	"pad-analyzer/internal/storage"
)

// ProviderService manages AI provider configuration, authentication, and connectivity.
type ProviderService struct {
	auth        *ai.GitHubAuth
	copilotAuth *ai.CopilotAuth
	factory     *ai.ProviderFactory
}

func NewProviderService(auth *ai.GitHubAuth, copilotAuth *ai.CopilotAuth, factory *ai.ProviderFactory) *ProviderService {
	return &ProviderService{auth: auth, copilotAuth: copilotAuth, factory: factory}
}

func (s *ProviderService) ListProviders(ctx context.Context, scope string) (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	for _, meta := range ai.AvailableProviders() {
		p := ai.GetMetadataProvider(meta.ID)
		if p == nil {
			continue
		}

		// OAuth device-flow tokens are global (not yet per-user); manual API keys
		// and the copilot PAT are resolved under the caller's scope.
		configured := false
		switch meta.ID {
		case "github-models":
			ok, _ := storage.HasApiKeyScoped("", "github-models-token")
			configured = ok
		case "copilot":
			oauthOk, _ := storage.HasApiKeyScoped("", "copilot-oauth-token")
			patOk, _ := storage.HasApiKeyScoped(scope, "copilot")
			configured = oauthOk || patOk
		default:
			ok, _ := storage.HasApiKeyScoped(scope, meta.ID)
			configured = ok
		}

		modelInfos, mErr := p.Models(ctx)
		if mErr != nil {
			logger.Warn("list provider models", "provider", meta.ID, "error", mErr)
		}
		if configured {
			if realProvider, err := s.factory.For(scope, meta.ID); err == nil {
				if dynamicModels, err := realProvider.Models(ctx); err == nil {
					modelInfos = dynamicModels
				} else {
					logger.Warn("list dynamic provider models", "provider", meta.ID, "error", err)
				}
			}
		}

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

func (s *ProviderService) TestProviderConnection(ctx context.Context, scope, providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestProviderConnection", &err)

	provider, err := s.factory.For(scope, providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: err.Error()}, nil
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
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

func (s *ProviderService) StartGitHubAuth(ctx context.Context) (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartGitHubAuth", &err)
	return s.auth.StartDeviceFlow(ctx)
}

func (s *ProviderService) PollGitHubAuth(ctx context.Context, scope string, deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollGitHubAuth", &err)

	result, err = s.auth.PollToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := storage.SaveApiKeyScoped(scope, "github-models-token", result.Token); saveErr != nil {
			logger.Error("failed to save github token", "error", saveErr)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeGitHubAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	return storage.DeleteApiKeyScoped(scope, "github-models-token")
}

func (s *ProviderService) GetGitHubUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)

	token, err := storage.GetApiKeyScoped(scope, "github-models-token")
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}

// --- GitHub Copilot OAuth ---

func (s *ProviderService) StartCopilotAuth(ctx context.Context) (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartCopilotAuth", &err)
	return s.copilotAuth.StartDeviceFlow(ctx)
}

func (s *ProviderService) PollCopilotAuth(ctx context.Context, scope string, deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollCopilotAuth", &err)

	result, err = s.copilotAuth.PollToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := storage.SaveApiKeyScoped(scope, "copilot-oauth-token", result.Token); saveErr != nil {
			logger.Error("failed to save copilot oauth token", "error", saveErr)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeCopilotAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeCopilotAuth", &err)
	return storage.DeleteApiKeyScoped(scope, "copilot-oauth-token")
}

func (s *ProviderService) GetCopilotUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)

	token, err := storage.GetApiKeyScoped(scope, "copilot-oauth-token")
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}

