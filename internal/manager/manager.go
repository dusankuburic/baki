package manager

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// These are injected at build time via -ldflags:
//
//	-X main.Version=1.0.0 -X main.BuildDate=2026-05-24 -X main.GitCommit=abc1234
var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

type App struct {
	ctx       context.Context
	notifier  service.Notifier
	storage   storageif.StorageBackend // nil in local mode; postgres backend in cloud mode
	settings  *storage.SettingsStore
	flow      *service.FlowService
	analysis  *service.AnalysisService
	chat      *service.ChatService
	providers *service.ProviderService
	export    *service.ExportService
}

// NewApp creates the application manager. Pass a non-nil storageBackend to
// enable cloud/database mode; pass nil to use the default local filesystem.
func NewApp(storageBackend storageif.StorageBackend) *App {
	return &App{storage: storageBackend}
}

func (a *App) Init(ctx context.Context, notifier service.Notifier) {
	a.ctx = ctx
	a.notifier = notifier

	configDir, err := storage.ConfigDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get config dir: %v\n", err)
		return
	}

	if err := logger.Init(configDir, false); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
	}

	settings, err := storage.NewSettingsStore()
	if err != nil {
		logger.Error("failed to init settings store", "error", err)
		return
	}
	a.settings = settings

	copilotAuth := ai.NewCopilotAuth()
	factory := ai.NewProviderFactory(storage.GetApiKey, copilotAuth)
	auth := ai.NewGitHubAuth()
	demo := ai.NewDemoLimiter(configDir)

	a.flow = service.NewFlowService(ctx, notifier, settings)
	a.analysis = service.NewAnalysisService(ctx, notifier, a.flow, settings)
	a.export = service.NewExportService(ctx, notifier, a.flow, a.analysis)
	a.providers = service.NewProviderService(ctx, auth, copilotAuth, factory)
	a.chat = service.NewChatService(ctx, notifier, configDir, a.flow, a.analysis, settings, factory, demo)

	logger.Info("app initialized")
}

// StorageBackend returns the active storage backend (nil in local mode).
func (a *App) StorageBackend() storageif.StorageBackend {
	return a.storage
}

// --- library (cloud-mode flow storage) ---

// ListLibraryFlows returns flows visible to the requesting user.
// userID scopes results to personally-owned flows; orgID additionally includes org-shared flows.
func (a *App) ListLibraryFlows(userID, orgID, query string, limit, offset int) (docs []*storageif.FlowDocument, err error) {
	defer logger.Guard("App.ListLibraryFlows", &err)
	if a.storage == nil {
		return []*storageif.FlowDocument{}, nil
	}
	return a.storage.ListFlows(a.ctx, storageif.FlowFilter{
		UserID:         userID,
		OrganizationID: orgID,
		Query:          query,
		Limit:          limit,
		Offset:         offset,
	})
}

// GetLibraryFlow loads a single flow by ID, returning ErrNotFound if absent.
// In cloud mode the caller should verify ownership after loading.
func (a *App) GetLibraryFlow(flowID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("App.GetLibraryFlow", &err)
	if a.storage == nil {
		return nil, storageif.ErrNotFound
	}
	return a.storage.LoadFlow(a.ctx, flowID)
}

// CreateLibraryFlow persists a new flow owned by ownerID.
func (a *App) CreateLibraryFlow(ownerID, orgID string, doc storageif.FlowDocument) (saved *storageif.FlowDocument, err error) {
	defer logger.Guard("App.CreateLibraryFlow", &err)
	if a.storage == nil {
		return nil, fmt.Errorf("cloud storage not available in local mode")
	}
	doc.OwnerID = ownerID
	doc.OrganizationID = orgID
	if err := a.storage.SaveFlow(a.ctx, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// UpdateLibraryFlow saves changes to an existing flow document.
// The handler is responsible for verifying ownership before calling.
func (a *App) UpdateLibraryFlow(doc *storageif.FlowDocument) (err error) {
	defer logger.Guard("App.UpdateLibraryFlow", &err)
	if a.storage == nil {
		return fmt.Errorf("cloud storage not available in local mode")
	}
	return a.storage.SaveFlow(a.ctx, doc)
}

// DeleteLibraryFlow removes a flow. The handler should verify ownership before calling.
func (a *App) DeleteLibraryFlow(flowID string) (err error) {
	defer logger.Guard("App.DeleteLibraryFlow", &err)
	if a.storage == nil {
		return fmt.Errorf("cloud storage not available in local mode")
	}
	return a.storage.DeleteFlow(a.ctx, flowID)
}

// --- system ---

func (a *App) GetSettings() (settings *models.AppSettings, err error) {
	defer logger.Guard("App.GetSettings", &err)
	if a.settings == nil {
		return models.DefaultSettings(), nil
	}
	return a.settings.Get(), nil
}

func (a *App) UpdateSettings(s models.AppSettings) (err error) {
	defer logger.Guard("App.UpdateSettings", &err)
	if a.settings == nil {
		return fmt.Errorf("settings store not initialized")
	}
	if err = a.settings.Update(s); err != nil {
		return err
	}
	a.notifier.Emit("settings:changed", s)
	return nil
}

func (a *App) AppInfo() (info *models.AppInfo, err error) {
	defer logger.Guard("App.AppInfo", &err)
	return &models.AppInfo{
		Version:   Version,
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		BuildDate: BuildDate,
		GitCommit: GitCommit,
	}, nil
}

func (a *App) LogError(payload models.FrontendError) {
	logger.Error("frontend error",
		"message", payload.Message,
		"stack", payload.Stack,
		"componentStack", payload.ComponentStack,
		"url", payload.URL,
	)
}

// --- provider keys ---

func (a *App) SaveApiKey(provider string, key string) (err error) {
	defer logger.Guard("App.SaveApiKey", &err)
	return storage.SaveApiKey(provider, key)
}

func (a *App) HasApiKey(provider string) (result bool, err error) {
	defer logger.Guard("App.HasApiKey", &err)
	return storage.HasApiKey(provider)
}

func (a *App) DeleteApiKey(provider string) (err error) {
	defer logger.Guard("App.DeleteApiKey", &err)
	return storage.DeleteApiKey(provider)
}

// --- flow ---

func (a *App) LoadFlowFromPath(path string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFromPath", &err)
	return a.flow.LoadFlowFromPath(path)
}

func (a *App) LoadFlowFolder(path string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFolder", &err)
	return a.flow.LoadFlowFolder(path)
}

func (a *App) LoadFlowFiles(files map[string]string, rootName string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFiles", &err)
	return a.flow.LoadFlowFiles(files, rootName)
}

func (a *App) RecentFiles() (files []models.RecentFile, err error) {
	defer logger.Guard("App.RecentFiles", &err)
	return a.flow.RecentFiles()
}

func (a *App) RemoveRecentFile(path string) (err error) {
	defer logger.Guard("App.RemoveRecentFile", &err)
	return a.flow.RemoveRecentFile(path)
}

func (a *App) ClearRecentFiles() (err error) {
	defer logger.Guard("App.ClearRecentFiles", &err)
	return a.flow.ClearRecentFiles()
}

func (a *App) RevealInFileManager(path string) (err error) {
	defer logger.Guard("App.RevealInFileManager", &err)
	return a.flow.RevealInFileManager(path)
}

func (a *App) SearchFlow(id string, q models.SearchQuery) (r *models.SearchResults, err error) {
	defer logger.Guard("App.SearchFlow", &err)
	return a.flow.SearchFlow(id, q)
}

func (a *App) GetSourceFiles() (result []models.SourceFileInfo, err error) {
	defer logger.Guard("App.GetSourceFiles", &err)
	return a.flow.GetSourceFiles()
}

func (a *App) ReadSourceFiles(names []string) (result map[string]string, err error) {
	defer logger.Guard("App.ReadSourceFiles", &err)
	return a.flow.ReadSourceFiles(names)
}

func (a *App) OnFileOpenFromSystem(path string) {
	if a.flow != nil {
		a.flow.OnFileOpenFromSystem(path)
	}
}

// --- providers ---

func (a *App) ListProviders() (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)
	return a.providers.ListProviders()
}

func (a *App) TestProviderConnection(id string) (r *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestProviderConnection", &err)
	return a.providers.TestProviderConnection(id)
}

func (a *App) StartGitHubAuth() (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartGitHubAuth", &err)
	return a.providers.StartGitHubAuth()
}

func (a *App) PollGitHubAuth(code string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollGitHubAuth", &err)
	return a.providers.PollGitHubAuth(code)
}

func (a *App) RevokeGitHubAuth() (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	return a.providers.RevokeGitHubAuth()
}

func (a *App) GetGitHubUser() (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)
	return a.providers.GetGitHubUser()
}

// --- copilot auth ---

func (a *App) StartCopilotAuth() (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartCopilotAuth", &err)
	return a.providers.StartCopilotAuth()
}

func (a *App) PollCopilotAuth(code string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollCopilotAuth", &err)
	return a.providers.PollCopilotAuth(code)
}

func (a *App) RevokeCopilotAuth() (err error) {
	defer logger.Guard("App.RevokeCopilotAuth", &err)
	return a.providers.RevokeCopilotAuth()
}

func (a *App) GetCopilotUser() (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)
	return a.providers.GetCopilotUser()
}

// --- chat ---

func (a *App) StreamChatMessage(req models.ChatRequest) (id string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)
	return a.chat.StreamChatMessage(req)
}

func (a *App) BeginStream(id string) {
	if a.chat != nil {
		a.chat.BeginStream(id)
	}
}

func (a *App) CancelStream(id string) {
	if a.chat != nil {
		a.chat.CancelStream(id)
	}
}

func (a *App) GetConversation(flowID, blockId string) (conv *models.ConversationFile, err error) {
	defer logger.Guard("App.GetConversation", &err)
	return a.chat.GetConversation(flowID, blockId)
}

func (a *App) SaveConversation(flowID, blockId string, msgs []models.ChatMessage) (err error) {
	defer logger.Guard("App.SaveConversation", &err)
	return a.chat.SaveConversation(flowID, blockId, msgs)
}

func (a *App) ClearConversation(flowID, blockId string) (err error) {
	defer logger.Guard("App.ClearConversation", &err)
	return a.chat.ClearConversation(flowID, blockId)
}

func (a *App) ExportConversation(flowID, blockId string, path string) (err error) {
	defer logger.Guard("App.ExportConversation", &err)
	return a.chat.ExportConversation(flowID, blockId, path)
}

func (a *App) GetDemoRemaining() (remaining int, err error) {
	defer logger.Guard("App.GetDemoRemaining", &err)
	return a.chat.GetDemoRemaining()
}

func (a *App) PreviewContext(req models.ChatRequest) (result *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)
	return a.chat.PreviewContext(req)
}

func (a *App) GetSuggestedPrompts(hasBlock bool, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)
	return a.chat.GetSuggestedPrompts(hasBlock, hasFindings)
}

// --- analysis ---

func (a *App) AnalyzeFlow() (report *models.AnalysisReport, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)
	return a.analysis.AnalyzeFlow()
}

func (a *App) GetVariableLineage(v string) (history *models.VariableHistory, err error) {
	defer logger.Guard("App.GetVariableLineage", &err)
	return a.analysis.GetVariableLineage(v)
}

func (a *App) GetExecutionGraph() (graph *models.GraphData, err error) {
	defer logger.Guard("App.GetExecutionGraph", &err)
	return a.analysis.GetExecutionGraph()
}

func (a *App) GetRules() []models.Rule {
	if a.analysis == nil {
		return nil
	}
	return a.analysis.GetRules()
}

func (a *App) SetRuleEnabled(id string, enabled bool) (err error) {
	defer logger.Guard("App.SetRuleEnabled", &err)
	return a.analysis.SetRuleEnabled(id, enabled)
}

func (a *App) UpdateRuleConfig(id string, config models.RuleConfig) (err error) {
	defer logger.Guard("App.UpdateRuleConfig", &err)
	return a.analysis.UpdateRuleConfig(id, config)
}

// --- export ---

func (a *App) CompareCurrentWith(path string) (diff *models.FlowDiff, err error) {
	defer logger.Guard("App.CompareCurrentWith", &err)
	return a.export.CompareCurrentWith(path)
}

func (a *App) ExportMarkdown(path string) (content []byte, err error) {
	defer logger.Guard("App.ExportMarkdown", &err)
	return a.export.ExportMarkdown(path)
}

func (a *App) ExportPDF(path string) (content []byte, err error) {
	defer logger.Guard("App.ExportPDF", &err)
	return a.export.ExportPDF(path)
}
