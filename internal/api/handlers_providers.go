package api

import (
	"encoding/json"
	"net/http"
)

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
