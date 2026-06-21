package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Machine API tokens (personal access tokens). These let CI and other automation
// call the API as the issuing user without an interactive login. They require a
// storage backend (cloud mode); the raw token is returned exactly once, at
// creation, and only its hash is ever stored.

func (h *AuthHandler) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	raw, hash, err := auth.GenerateAPIToken()
	if err != nil {
		render.Error(w, fmt.Errorf("generate token: %w", err), http.StatusInternalServerError)
		return
	}

	tok := &storageif.APIToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      req.Name,
		TokenHash: hash,
		CreatedAt: time.Now().UTC(),
	}
	if req.ExpiresInDays > 0 {
		exp := tok.CreatedAt.AddDate(0, 0, req.ExpiresInDays)
		tok.ExpiresAt = &exp
	}

	if err := h.backend.CreateAPIToken(r.Context(), tok); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionTokenCreate, "api_token", tok.ID,
		map[string]string{"name": tok.Name})

	// The raw token is shown exactly once here; afterwards only its hash exists.
	render.JSON(w, map[string]any{
		"id":        tok.ID,
		"name":      tok.Name,
		"token":     raw,
		"createdAt": tok.CreatedAt,
		"expiresAt": tok.ExpiresAt,
	})
}

func (h *AuthHandler) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	tokens, err := h.backend.ListAPITokens(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if tokens == nil {
		tokens = []*storageif.APIToken{}
	}
	// APIToken.TokenHash is json:"-", so the hash is never serialized here.
	render.JSON(w, tokens)
}

func (h *AuthHandler) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("API tokens require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		render.Error(w, fmt.Errorf("token id is required"), http.StatusBadRequest)
		return
	}

	if err := h.backend.DeleteAPIToken(r.Context(), userID, id); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionTokenRevoke, "api_token", id, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}
