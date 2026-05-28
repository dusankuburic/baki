package api

import (
	"encoding/json"
	"net/http"
)

// @Summary List AI providers
// @Description Returns a list of supported AI providers and their current status.
// @Tags providers
// @Produce json
// @Success 200 {array} models.ProviderInfo
// @Failure 500 {object} map[string]string
// @Router /api/providers/list [get]
func (rt *Router) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := rt.app.ListProviders()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, providers)
}

// @Summary Test provider connection
// @Description Tests the connection to a specific AI provider.
// @Tags providers
// @Accept json
// @Produce json
// @Param request body object{provider=string} true "Test Connection Request"
// @Success 200 {object} models.ProviderTestResult
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/providers/test [post]
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

// @Summary Start GitHub OAuth
// @Description Initiates the GitHub device flow authentication.
// @Tags providers
// @Produce json
// @Success 200 {object} ai.DeviceAuthResponse
// @Failure 500 {object} map[string]string
// @Router /api/providers/github/start [post]
func (rt *Router) handleStartGitHubAuth(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.StartGitHubAuth()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

// @Summary Poll GitHub OAuth
// @Description Polls GitHub for the status of the device flow authentication.
// @Tags providers
// @Accept json
// @Produce json
// @Param request body object{deviceCode=string} true "Poll Auth Request"
// @Success 200 {object} ai.GitHubAuthResult
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/providers/github/poll [post]
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

// @Summary Revoke GitHub OAuth
// @Description Revokes the GitHub authentication for the current user.
// @Tags providers
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/providers/github/revoke [post]
func (rt *Router) handleRevokeGitHubAuth(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.RevokeGitHubAuth(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Get GitHub user info
// @Description Returns information about the currently authenticated GitHub user.
// @Tags providers
// @Produce json
// @Success 200 {object} ai.GitHubUser
// @Failure 500 {object} map[string]string
// @Router /api/providers/github/user [get]
func (rt *Router) handleGetGitHubUser(w http.ResponseWriter, r *http.Request) {
	user, err := rt.app.GetGitHubUser()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, user)
}

// @Summary Start Copilot OAuth
// @Description Initiates the GitHub Copilot device flow authentication.
// @Tags providers
// @Produce json
// @Success 200 {object} ai.DeviceAuthResponse
// @Failure 500 {object} map[string]string
// @Router /api/providers/copilot/start [post]
func (rt *Router) handleStartCopilotAuth(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.StartCopilotAuth()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

// @Summary Poll Copilot OAuth
// @Description Polls GitHub for the status of the Copilot device flow authentication.
// @Tags providers
// @Accept json
// @Produce json
// @Param request body object{deviceCode=string} true "Poll Copilot Auth Request"
// @Success 200 {object} ai.GitHubAuthResult
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/providers/copilot/poll [post]
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

// @Summary Revoke Copilot OAuth
// @Description Revokes the GitHub Copilot authentication for the current user.
// @Tags providers
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/providers/copilot/revoke [post]
func (rt *Router) handleRevokeCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.RevokeCopilotAuth(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Get Copilot user info
// @Description Returns information about the currently authenticated Copilot user.
// @Tags providers
// @Produce json
// @Success 200 {object} ai.GitHubUser
// @Failure 500 {object} map[string]string
// @Router /api/providers/copilot/user [get]
func (rt *Router) handleGetCopilotUser(w http.ResponseWriter, r *http.Request) {
	user, err := rt.app.GetCopilotUser()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, user)
}
