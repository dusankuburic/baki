package api

import (
	"encoding/json"
	"net/http"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
)

func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.URL.Path != "/api/events" {
		// Most RPC-style calls will be POST for simplicity in handling bodies
		// but some could be GET. For this implementation, we'll favor POST for anything with arguments.
	}

	switch r.URL.Path {
	// --- System ---
	case "/api/system/settings":
		if r.Method == http.MethodGet {
			rt.handleGetSettings(w, r)
		} else {
			rt.handleUpdateSettings(w, r)
		}
	case "/api/system/info":
		rt.handleAppInfo(w, r)
	case "/api/system/log-error":
		rt.handleLogError(w, r)

	// --- Keys ---
	case "/api/keys/save":
		rt.handleSaveApiKey(w, r)
	case "/api/keys/has":
		rt.handleHasApiKey(w, r)
	case "/api/keys/delete":
		rt.handleDeleteApiKey(w, r)

	// --- Flow ---
	case "/api/flow/load-path":
		rt.handleLoadFlowFromPath(w, r)
	case "/api/flow/load-folder":
		rt.handleLoadFlowFolder(w, r)
	case "/api/flow/recent":
		rt.handleRecentFiles(w, r)
	case "/api/flow/remove-recent":
		rt.handleRemoveRecentFile(w, r)
	case "/api/flow/clear-recent":
		rt.handleClearRecentFiles(w, r)
	case "/api/flow/reveal":
		rt.handleRevealInFileManager(w, r)
	case "/api/flow/search":
		rt.handleSearchFlow(w, r)
	case "/api/flow/source-files":
		rt.handleGetSourceFiles(w, r)
	case "/api/flow/read-sources":
		rt.handleReadSourceFiles(w, r)
	case "/api/flow/open-from-system":
		rt.handleOnFileOpenFromSystem(w, r)

	// --- Providers ---
	case "/api/providers/list":
		rt.handleListProviders(w, r)
	case "/api/providers/test":
		rt.handleTestProviderConnection(w, r)
	case "/api/providers/github/start":
		rt.handleStartGitHubAuth(w, r)
	case "/api/providers/github/poll":
		rt.handlePollGitHubAuth(w, r)
	case "/api/providers/github/revoke":
		rt.handleRevokeGitHubAuth(w, r)
	case "/api/providers/github/user":
		rt.handleGetGitHubUser(w, r)
	case "/api/providers/copilot/start":
		rt.handleStartCopilotAuth(w, r)
	case "/api/providers/copilot/poll":
		rt.handlePollCopilotAuth(w, r)
	case "/api/providers/copilot/revoke":
		rt.handleRevokeCopilotAuth(w, r)
	case "/api/providers/copilot/user":
		rt.handleGetCopilotUser(w, r)

	// --- Chat ---
	case "/api/chat/stream":
		rt.handleStreamChatMessage(w, r)
	case "/api/chat/begin":
		rt.handleBeginStream(w, r)
	case "/api/chat/cancel":
		rt.handleCancelStream(w, r)
	case "/api/chat/get":
		rt.handleGetConversation(w, r)
	case "/api/chat/save":
		rt.handleSaveConversation(w, r)
	case "/api/chat/clear":
		rt.handleClearConversation(w, r)
	case "/api/chat/export":
		rt.handleExportConversation(w, r)
	case "/api/chat/demo-remaining":
		rt.handleGetDemoRemaining(w, r)
	case "/api/chat/preview-context":
		rt.handlePreviewContext(w, r)
	case "/api/chat/suggested-prompts":
		rt.handleGetSuggestedPrompts(w, r)

	// --- Analysis ---
	case "/api/analysis/analyze":
		rt.handleAnalyzeFlow(w, r)
	case "/api/analysis/lineage":
		rt.handleGetVariableLineage(w, r)
	case "/api/analysis/graph":
		rt.handleGetExecutionGraph(w, r)
	case "/api/analysis/rules":
		rt.handleGetRules(w, r)
	case "/api/analysis/rule/enabled":
		rt.handleSetRuleEnabled(w, r)
	case "/api/analysis/rule/config":
		rt.handleUpdateRuleConfig(w, r)

	// --- Export ---
	case "/api/export/compare":
		rt.handleCompareCurrentWith(w, r)
	case "/api/export/markdown":
		rt.handleExportMarkdown(w, r)
	case "/api/export/pdf":
		rt.handleExportPDF(w, r)

	default:
		http.NotFound(w, r)
	}
}

// Helpers

func (rt *Router) sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (rt *Router) sendError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Handler implementations

func (rt *Router) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := rt.app.GetSettings()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, settings)
}

func (rt *Router) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var s models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		logger.Error("failed to decode settings", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.UpdateSettings(s); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleAppInfo(w http.ResponseWriter, r *http.Request) {
	info, err := rt.app.AppInfo()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, info)
}

func (rt *Router) handleLogError(w http.ResponseWriter, r *http.Request) {
	var payload models.FrontendError
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.LogError(payload)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleSaveApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.SaveApiKey(req.Provider, req.Key); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleHasApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	has, err := rt.app.HasApiKey(req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, has)
}

func (rt *Router) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.DeleteApiKey(req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleLoadFlowFromPath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("failed to decode load flow from path request", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	logger.Info("loading flow from path", "path", req.Path)
	doc, err := rt.app.LoadFlowFromPath(req.Path)
	if err != nil {
		logger.Error("failed to load flow from path", "path", req.Path, "error", err)
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, doc)
}

func (rt *Router) handleLoadFlowFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("failed to decode load flow folder request", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	logger.Info("loading flow folder", "path", req.Path)
	doc, err := rt.app.LoadFlowFolder(req.Path)
	if err != nil {
		logger.Error("failed to load flow folder", "path", req.Path, "error", err)
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, doc)
}

func (rt *Router) handleRecentFiles(w http.ResponseWriter, r *http.Request) {
	files, err := rt.app.RecentFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

func (rt *Router) handleRemoveRecentFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.RemoveRecentFile(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleClearRecentFiles(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.ClearRecentFiles(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleRevealInFileManager(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.RevealInFileManager(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleSearchFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string             `json:"id"`
		Query models.SearchQuery `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.SearchFlow(req.ID, req.Query)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleGetSourceFiles(w http.ResponseWriter, r *http.Request) {
	files, err := rt.app.GetSourceFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

func (rt *Router) handleReadSourceFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.ReadSourceFiles(req.Files)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleOnFileOpenFromSystem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.OnFileOpenFromSystem(req.Path)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := rt.app.ListProviders()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, providers)
}

func (rt *Router) handleTestProviderConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.TestProviderConnection(req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleStartGitHubAuth(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.StartGitHubAuth()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handlePollGitHubAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.PollGitHubAuth(req.DeviceCode)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleRevokeGitHubAuth(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.RevokeGitHubAuth(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleGetGitHubUser(w http.ResponseWriter, r *http.Request) {
	user, err := rt.app.GetGitHubUser()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, user)
}

func (rt *Router) handleStartCopilotAuth(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.StartCopilotAuth()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handlePollCopilotAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.PollCopilotAuth(req.DeviceCode)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleRevokeCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.RevokeCopilotAuth(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleGetCopilotUser(w http.ResponseWriter, r *http.Request) {
	user, err := rt.app.GetCopilotUser()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, user)
}

func (rt *Router) handleStreamChatMessage(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	id, err := rt.app.StreamChatMessage(req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, id)
}

func (rt *Router) handleBeginStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"streamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.BeginStream(req.ID)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleCancelStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"streamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.CancelStream(req.ID)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	conv, err := rt.app.GetConversation(req.FlowID, req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, conv)
}

func (rt *Router) handleSaveConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string               `json:"flowId"`
		Provider string               `json:"provider"`
		Messages []models.ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.SaveConversation(req.FlowID, req.Provider, req.Messages); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleClearConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ClearConversation(req.FlowID, req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleExportConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ExportConversation(req.FlowID, req.Provider, req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleGetDemoRemaining(w http.ResponseWriter, r *http.Request) {
	remaining, err := rt.app.GetDemoRemaining()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, remaining)
}

func (rt *Router) handlePreviewContext(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.PreviewContext(req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleGetSuggestedPrompts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HasBlock    bool `json:"hasBlock"`
		HasFindings bool `json:"hasFindings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	prompts, err := rt.app.GetSuggestedPrompts(req.HasBlock, req.HasFindings)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, prompts)
}

func (rt *Router) handleAnalyzeFlow(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.AnalyzeFlow()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variable string `json:"varName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	history, err := rt.app.GetVariableLineage(req.Variable)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, history)
}

func (rt *Router) handleGetExecutionGraph(w http.ResponseWriter, r *http.Request) {
	graph, err := rt.app.GetExecutionGraph()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, graph)
}

func (rt *Router) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := rt.app.GetRules()
	rt.sendJSON(w, rules)
}

func (rt *Router) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.SetRuleEnabled(req.ID, req.Enabled); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string            `json:"id"`
		Config models.RuleConfig `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.UpdateRuleConfig(req.ID, req.Config); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleCompareCurrentWith(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	diff, err := rt.app.CompareCurrentWith(req.Path)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, diff)
}

func (rt *Router) handleExportMarkdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ExportMarkdown(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ExportPDF(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
