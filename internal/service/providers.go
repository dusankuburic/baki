package service

import (
	"context"
	"errors"
	"strings"
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
	models []ai.ModelInfo
	// defaultModel is the CONFIGURED provider's default (a user's Azure
	// deployment name), which can differ from the metadata provider's.
	defaultModel string
	expiresAt    time.Time
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
func (s *ProviderService) dynamicModels(ctx context.Context, scope, providerID string) (models []ai.ModelInfo, defaultModel string, ok bool) {
	key := scope + "\x00" + providerID
	s.modelMu.Lock()
	if e, ok := s.modelCache[key]; ok && time.Now().Before(e.expiresAt) {
		s.modelMu.Unlock()
		return e.models, e.defaultModel, true
	}
	s.modelMu.Unlock()

	realProvider, err := s.factory.For(scope, providerID)
	if err != nil {
		return nil, "", false
	}
	models, err = realProvider.Models(ctx)
	if err != nil {
		logger.Warn("list dynamic provider models", "provider", providerID, "error", err)
		return nil, "", false
	}
	defaultModel = realProvider.DefaultModel()
	s.modelMu.Lock()
	s.modelCache[key] = modelCacheEntry{models: models, defaultModel: defaultModel, expiresAt: time.Now().Add(dynamicModelTTL)}
	s.modelMu.Unlock()
	return models, defaultModel, true
}

// InvalidateModels drops the cached live listing for one (scope, provider) so
// a configuration change (new Azure deployment names, rotated credential)
// shows up on the next listing instead of after dynamicModelTTL.
func (s *ProviderService) InvalidateModels(scope, providerID string) {
	s.modelMu.Lock()
	delete(s.modelCache, scope+"\x00"+providerID)
	s.modelMu.Unlock()
}

// chooseDefaultModel picks the model the UI preselects for a configured
// provider: the provider's own default when the live listing has it, else the
// metadata fallback when it has that, else the first chat-capable entry
// (skipping agents and embedding deployments). A user-named Azure AI Foundry
// resource rarely has a deployment called "gpt-4.1".
func chooseDefaultModel(preferred string, models []ai.ModelInfo, fallback string) string {
	has := func(id string) bool {
		for _, m := range models {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if preferred != "" && has(preferred) {
		return preferred
	}
	if fallback != "" && has(fallback) {
		return fallback
	}
	for _, m := range models {
		if !ai.IsAgentModel(m.ID) && !isEmbeddingModelID(m.ID) {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	if fallback != "" {
		return fallback
	}
	return preferred
}

// isEmbeddingModelID is the heuristic that keeps an embedding deployment from
// being offered as the chat default.
func isEmbeddingModelID(id string) bool {
	return strings.Contains(strings.ToLower(id), "embed")
}

// connectionTestModels lists the models the connection test tries: the
// provider's default and free model — but when the live listing (5 s budget)
// does not contain the default, the first chat-capable listed model goes
// first, so a resource whose deployments carry custom names still passes.
func (s *ProviderService) connectionTestModels(ctx context.Context, provider ai.Provider) []string {
	def := provider.DefaultModel()
	candidates := []string{def}
	if fm := provider.FreeModel(); fm != "" && fm != def {
		candidates = append(candidates, fm)
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	live, err := provider.Models(lctx)
	if err != nil || len(live) == 0 {
		return candidates
	}
	for _, m := range live {
		if m.ID == def {
			return candidates
		}
	}
	if pick := chooseDefaultModel("", live, ""); pick != "" && pick != def {
		return append([]string{pick}, candidates...)
	}
	return candidates
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
		defaultModel := p.DefaultModel()
		if configured {
			if dynamicModels, liveDefault, ok := s.dynamicModels(ctx, scope, meta.ID); ok {
				modelInfos = dynamicModels
				// The metadata provider's default is a catalogue name; for a
				// provider whose models are the user's own deployments (Azure
				// AI Foundry) the default must be one the listing really has.
				defaultModel = chooseDefaultModel(liveDefault, dynamicModels, defaultModel)
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
				SupportsTools:  p.SupportsTools(),
			}
		}

		providers = append(providers, models.ProviderInfo{
			ID:           meta.ID,
			Name:         meta.Name,
			Configured:   configured,
			Models:       modelDetails,
			DefaultModel: defaultModel,
			ContextLimit: p.ContextLimit(),
			AuthType:     meta.AuthType,
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

	testModels := s.connectionTestModels(ctx, provider)

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

// --- Azure AI Foundry configuration ---

// GetAzureFoundryConfig returns the caller's stored Azure AI Foundry document
// with the secrets removed. A missing or unreadable document reads as
// unconfigured (the user re-enters it).
func (s *ProviderService) GetAzureFoundryConfig(scope string) (cfg ai.AzureFoundryPublicConfig, err error) {
	defer logger.Guard("App.GetAzureFoundryConfig", &err)
	raw, getErr := s.secrets.Get(scope, ai.AzureFoundryProviderID)
	if getErr != nil || raw == "" {
		return ai.AzureFoundryPublicConfig{}, nil
	}
	parsed, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		logger.Warn("azure ai foundry: stored configuration unreadable", "error", parseErr)
		return ai.AzureFoundryPublicConfig{}, nil
	}
	return parsed.Public(), nil
}

// SaveAzureFoundryConfig validates and stores the document. A request that
// omits the secret keeps the stored one (same auth mode), so editing the
// endpoint or a deployment name never asks the user to re-enter a key.
// Validation failures are *ai.AzureConfigError values carrying a client code.
func (s *ProviderService) SaveAzureFoundryConfig(scope string, cfg ai.AzureFoundryConfig) (err error) {
	defer logger.Guard("App.SaveAzureFoundryConfig", &err)
	cfg.Normalize()
	if !cfg.HasSecret() {
		if raw, getErr := s.secrets.Get(scope, ai.AzureFoundryProviderID); getErr == nil && raw != "" {
			if prev, parseErr := ai.ParseAzureFoundryConfig(raw); parseErr == nil && prev.Auth.Mode == cfg.Auth.Mode {
				switch cfg.Auth.Mode {
				case ai.AzureAuthModeAPIKey:
					cfg.Auth.Key = prev.Auth.Key
				case ai.AzureAuthModeEntra:
					cfg.Auth.ClientSecret = prev.Auth.ClientSecret
				}
			}
		}
	}
	if err := ai.ValidateAzureFoundryConfig(cfg); err != nil {
		return err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return err
	}
	if err := s.secrets.Save(scope, ai.AzureFoundryProviderID, raw); err != nil {
		return err
	}
	s.InvalidateModels(scope, ai.AzureFoundryProviderID)
	ai.InvalidateAzureFoundryModels(cfg)
	return nil
}
