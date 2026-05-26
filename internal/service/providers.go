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
	ctx     context.Context
	auth    *ai.GitHubAuth
	factory *ai.ProviderFactory
}

func NewProviderService(ctx context.Context, auth *ai.GitHubAuth, factory *ai.ProviderFactory) *ProviderService {
	return &ProviderService{ctx: ctx, auth: auth, factory: factory}
}

func (s *ProviderService) ListProviders() (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	type provDef struct {
		id       string
		name     string
		authType string
		model    ai.Provider
	}

	defs := []provDef{
		{"claude", "Claude", "api_key", ai.NewClaudeProvider("")},
		{"openai", "OpenAI", "api_key", ai.NewOpenAIProvider("")},
		{"gemini", "Gemini", "api_key", ai.NewGeminiProvider("")},
		{"xai", "xAI (Grok)", "api_key", ai.NewXAIProvider("")},
		{"glm", "GLM (z.ai)", "api_key", ai.NewGLMProvider("")},
		{"github-models", "GitHub Models", "oauth", ai.NewGitHubModelsProvider("")},
	}

	for _, d := range defs {
		configured := false
		if d.id == "github-models" {
			ok, _ := storage.HasApiKey("github-models-token")
			configured = ok
		} else {
			ok, _ := storage.HasApiKey(d.id)
			configured = ok
		}

		modelInfos := d.model.Models()
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
			ID:           d.id,
			Name:         d.name,
			Configured:   configured,
			Models:       modelDetails,
			DefaultModel: d.model.DefaultModel(),
			ContextLimit: d.model.ContextLimit(),
			AuthType:     d.authType,
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
