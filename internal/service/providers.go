package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-core/logger"
	"pad-core/models"
)

// dynamicModelTTL bounds how long a CONFIGURED provider's live model list
// (fetched from the provider's API) is reused. Provider catalogs change
// rarely; without a cache, opening the settings panel twice fanned out an
// upstream HTTP call per provider per request, and a slow upstream stalled
// the response for up to the request timeout.
const dynamicModelTTL = 10 * time.Minute

// ProviderService manages AI provider configuration, authentication, and connectivity.
type ProviderService struct {
	auth        *ai.GitHubAuth
	copilotAuth *ai.CopilotAuth
	factory     *ai.ProviderFactory
	secrets     KeyStore

	modelMu    sync.Mutex
	modelCache map[string]modelCacheEntry // key: scope + "\x00" + providerID
}

type modelCacheEntry struct {
	models    []ai.ModelInfo
	expiresAt time.Time
}

func NewProviderService(auth *ai.GitHubAuth, copilotAuth *ai.CopilotAuth, factory *ai.ProviderFactory, secrets KeyStore) *ProviderService {
	return &ProviderService{
		auth:        auth,
		copilotAuth: copilotAuth,
		factory:     factory,
		secrets:     secrets,
		modelCache:  make(map[string]modelCacheEntry),
	}
}

// dynamicModels returns the configured provider's live model list, cached for
// dynamicModelTTL per (scope, provider). Static metadata providers are cheap
// (no network) and are NOT cached.
func (s *ProviderService) dynamicModels(ctx context.Context, scope, providerID string) ([]ai.ModelInfo, bool) {
	key := scope + "\x00" + providerID
	s.modelMu.Lock()
	if e, ok := s.modelCache[key]; ok && time.Now().Before(e.expiresAt) {
		s.modelMu.Unlock()
		return e.models, true
	}
	s.modelMu.Unlock()

	realProvider, err := s.factory.For(scope, providerID)
	if err != nil {
		return nil, false
	}
	models, err := realProvider.Models(ctx)
	if err != nil {
		logger.Warn("list dynamic provider models", "provider", providerID, "error", err)
		return nil, false
	}
	s.modelMu.Lock()
	s.modelCache[key] = modelCacheEntry{models: models, expiresAt: time.Now().Add(dynamicModelTTL)}
	s.modelMu.Unlock()
	return models, true
}

func (s *ProviderService) ListProviders(ctx context.Context, scope string) (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	for _, meta := range ai.AvailableProviders() {
		p := ai.GetMetadataProvider(meta.ID)
		if p == nil {
			continue
		}

		// OAuth device-flow tokens and manual API keys are both resolved under
		// the caller's scope (per-user in cloud mode).
		configured := false
		switch meta.ID {
		case "github-models":
			ok, _ := s.secrets.Has(scope, "github-models-token")
			configured = ok
		case "copilot":
			oauthOk, _ := s.secrets.Has(scope, "copilot-oauth-token")
			patOk, _ := s.secrets.Has(scope, "copilot")
			configured = oauthOk || patOk
		default:
			ok, _ := s.secrets.Has(scope, meta.ID)
			configured = ok
		}

		modelInfos, mErr := p.Models(ctx)
		if mErr != nil {
			logger.Warn("list provider models", "provider", meta.ID, "error", mErr)
		}
		if configured {
			if dynamicModels, ok := s.dynamicModels(ctx, scope, meta.ID); ok {
				modelInfos = dynamicModels
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
		if saveErr := s.secrets.Save(scope, "github-models-token", result.Token); saveErr != nil {
			logger.Error("failed to save github token", "error", saveErr)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeGitHubAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	return s.secrets.Delete(scope, "github-models-token")
}

func (s *ProviderService) GetGitHubUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)

	token, err := s.secrets.Get(scope, "github-models-token")
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
		if saveErr := s.secrets.Save(scope, "copilot-oauth-token", result.Token); saveErr != nil {
			logger.Error("failed to save copilot oauth token", "error", saveErr)
		}
		result.Token = ""
		// C-4: pre-warm the Copilot session-token cache so the first chat turn
		// doesn't pay the session-token exchange RTT on the critical path.
		// ListProviders will also warm it on next session load, but firing it
		// now covers the turn the user sends immediately after connecting.
		s.preWarmCopilot(scope)
	}

	return result, nil
}

// preWarmCopilot best-effort resolves the configured Copilot GitHub token and
// triggers a background session-token exchange so the cache is primed before
// the first chat turn. Fire-and-forget: errors are logged and swallowed (the
// token will be exchanged lazily on first Stream if this fails). The context
// is detached from the caller and bounded so a slow exchange can't linger.
func (s *ProviderService) preWarmCopilot(scope string) {
	if s.copilotAuth == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// OAuth device-flow token (per-user) first, then manual PAT.
		token, err := s.secrets.Get(scope, "copilot-oauth-token")
		if err != nil || token == "" {
			token, err = s.secrets.Get(scope, "copilot")
			if err != nil || token == "" {
				return // not configured — nothing to warm
			}
		}
		if _, err := s.copilotAuth.GetSessionToken(ctx, token); err != nil {
			logger.Warn("copilot session-token pre-warm failed", "error", err)
		}
	}()
}

func (s *ProviderService) RevokeCopilotAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeCopilotAuth", &err)
	return s.secrets.Delete(scope, "copilot-oauth-token")
}

func (s *ProviderService) GetCopilotUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)

	token, err := s.secrets.Get(scope, "copilot-oauth-token")
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}
