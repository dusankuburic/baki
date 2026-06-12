package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type FlowHandler struct {
	flowSvc     *service.FlowService
	docProvider service.DocumentProvider
	backend     storageif.StorageBackend
	security    *SecurityConfig
}

func NewFlowHandler(flowSvc *service.FlowService, docProvider service.DocumentProvider, backend storageif.StorageBackend, security *SecurityConfig) *FlowHandler {
	return &FlowHandler{
		flowSvc:     flowSvc,
		docProvider: docProvider,
		backend:     backend,
		security:    security,
	}
}

func (h *FlowHandler) handleUploadFlow(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
		return
	}
	metrics.RecordFlowOp("upload")
	var req struct {
		Name  string            `json:"name"`
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		render.Error(w, fmt.Errorf("no files uploaded"), http.StatusBadRequest)
		return
	}

	doc, err := h.flowSvc.LoadFlowFiles(r.Context(), req.Files, req.Name)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	if h.security.JWTEnabled {
		userID := h.security.CallerID(r)

		content, err := json.Marshal(doc)
		if err != nil {
			render.Error(w, fmt.Errorf("failed to marshal uploaded flow: %w", err), http.StatusInternalServerError)
			return
		}

		libDoc := storageif.FlowDocument{
			ID:      doc.ID,
			Name:    doc.Name,
			Content: content,
			OwnerID: userID,
			Metadata: storageif.FlowMetadata{
				BlockCount:   doc.Metadata.BlockCount,
				SubflowCount: doc.Metadata.SubflowCount,
			},
		}

		if h.backend != nil {
			if err := h.backend.SaveFlow(r.Context(), &libDoc); err != nil {
				render.Error(w, fmt.Errorf("failed to save uploaded flow: %w", err), http.StatusInternalServerError)
				return
			}
		}
	}

	render.JSON(w, doc)
}

func (h *FlowHandler) handleLoadFlowFromPath(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("loading from local paths is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
	metrics.RecordFlowOp("load_path")
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	doc, err := h.flowSvc.LoadFlowFromPath(req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, doc)
}

func (h *FlowHandler) handleLoadFlowFolder(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("loading from local folders is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
	metrics.RecordFlowOp("load_folder")
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	doc, err := h.flowSvc.LoadFlowFolder(req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, doc)
}

func (h *FlowHandler) handleRecentFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.JSON(w, []string{})
		return
	}
	files, err := h.flowSvc.RecentFiles()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, files)
}

func (h *FlowHandler) handleRemoveRecentFile(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.JSON(w, map[string]string{"status": "ok"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.flowSvc.RemoveRecentFile(req.Path); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *FlowHandler) handleClearRecentFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.JSON(w, map[string]string{"status": "ok"})
		return
	}
	if err := h.flowSvc.ClearRecentFiles(); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *FlowHandler) handleRevealInFileManager(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("forbidden in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.flowSvc.RevealInFileManager(req.Path); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *FlowHandler) handleSearchFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string             `json:"id"`
		Query models.SearchQuery `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.ID, "viewer")
	if !ok {
		return
	}
	
	res, err := h.flowSvc.SearchFlow(doc, req.Query)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *FlowHandler) handleGetSourceFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.JSON(w, []string{})
		return
	}
	doc := h.docProvider.CurrentDoc()
	files, err := h.flowSvc.GetSourceFiles(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, files)
}

func (h *FlowHandler) handleReadSourceFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("source file reading is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	doc := h.docProvider.CurrentDoc()
	res, err := h.flowSvc.ReadSourceFiles(doc, req.Files)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *FlowHandler) handleOnFileOpenFromSystem(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("opening from local paths is not supported in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	h.flowSvc.OnFileOpenFromSystem(req.Path)
	render.JSON(w, map[string]string{"status": "ok"})
}
