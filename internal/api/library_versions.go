package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
	"pad-core/parser"
)

// flowVersionView is the wire shape of a stored version; the storage struct is deliberately
// untagged (its nested FlowMetadata keys are read by SQL), so the camelCase is built here.
type flowVersionView struct {
	ID        string              `json:"id"`
	FlowID    string              `json:"flowId"`
	Version   int                 `json:"version"`
	Comment   string              `json:"comment"`
	Metadata  flowVersionMetaView `json:"metadata"`
	CreatedBy string              `json:"createdBy"`
	CreatedAt time.Time           `json:"createdAt"`
}

type flowVersionMetaView struct {
	BlockCount   int `json:"blockCount"`
	SubflowCount int `json:"subflowCount"`
	MaxDepth     int `json:"maxDepth"`
}

func toFlowVersionView(v *storageif.FlowVersion) flowVersionView {
	return flowVersionView{
		ID: v.ID, FlowID: v.FlowID, Version: v.Version, Comment: v.Comment,
		Metadata: flowVersionMetaView{
			BlockCount:   v.Metadata.BlockCount,
			SubflowCount: v.Metadata.SubflowCount,
			MaxDepth:     v.Metadata.MaxDepth,
		},
		CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
	}
}

func toFlowVersionViews(vs []*storageif.FlowVersion) []flowVersionView {
	out := make([]flowVersionView, 0, len(vs))
	for _, v := range vs {
		out = append(out, toFlowVersionView(v))
	}
	return out
}

// @Summary      List flow versions
// @Description  handleListFlowVersions returns the version history for a library flow.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Versions"
// @Router       /api/library/{id}/versions [get]
func (h *LibraryHandler) handleListFlowVersions(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []flowVersionView{})
		return
	}

	id := chi.URLParam(r, "id")
	limit, ok := parseIntParam(w, r.URL.Query().Get("limit"), "limit", 0)
	if !ok {
		return
	}
	versions, err := h.libSvc.ListFlowVersions(r.Context(), id, limit)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, toFlowVersionViews(versions))
}

// @Summary      Snapshot a flow version
// @Description  handleSaveFlowVersion snapshots the current flow content as a new version.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Created"
// @Router       /api/library/{id}/versions [post]
func (h *LibraryHandler) handleSaveFlowVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.libSvc.CanWrite(r.Context(), doc); err != nil {
		render.Error(w, err, 0)
		return
	}

	var req struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, fmt.Errorf("invalid request body: %w", err), http.StatusBadRequest)
		return
	}

	if len(req.Comment) > 500 {
		render.Error(w, fmt.Errorf("comment must be 500 characters or fewer"), http.StatusBadRequest)
		return
	}

	v, err := h.libSvc.SaveFlowVersion(r.Context(), id, doc, req.Comment, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowVersion, ResourceType: "flow", ResourceID: id, OrgID: doc.OrganizationID, Meta: map[string]string{"version": strconv.Itoa(v.Version)}})
	render.JSONStatus(w, http.StatusCreated, toFlowVersionView(v))
}

// @Summary      Get a flow version
// @Description  handleGetFlowVersion loads a specific historical version.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Version"
// @Router       /api/library/{id}/versions/{vn} [get]
func (h *LibraryHandler) handleGetFlowVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	v, err := h.libSvc.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, v)
}

// handleRestoreFlowVersion reverts the flow to a historical version's content,
// written as a NEW current version — so the restore is itself recoverable.
// @Summary      Restore a flow version
// @Description  handleRestoreFlowVersion reverts the current flow's content to a historical version's content. The historical content is written as a new current version (SaveFlow bumps the version atomically), so the restore is itself recoverable via the version history. Requires editor access.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Restored"
// @Router       /api/library/{id}/versions/{vn}/restore [post]
func (h *LibraryHandler) handleRestoreFlowVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if err := h.libSvc.CanWrite(r.Context(), doc); err != nil {
		render.Error(w, err, 0)
		return
	}

	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	historical, err := h.libSvc.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if len(historical.Content) == 0 {
		render.Error(w, fmt.Errorf("version %d has no content to restore", vn), http.StatusBadRequest)
		return
	}

	// SaveFlow's OCC guards against a concurrent edit; UpdateLibraryFlow
	// re-checks editor access and invalidates the cached search index.
	doc.Content = historical.Content
	if err := h.libSvc.UpdateLibraryFlow(r.Context(), doc); err != nil {
		render.Error(w, err, 0)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowRestore, ResourceType: "flow", ResourceID: id, OrgID: doc.OrganizationID, Meta: map[string]string{"restored_from": strconv.Itoa(vn), "version": strconv.Itoa(doc.Version)}})
	render.JSON(w, h.toLibraryFlow(r.Context(), doc, userID, h.libSvc.ResolveOwnerName(r.Context(), doc.OwnerID)))
}

// handleDiffFlowVersion returns the structural diff between a historical
// version and the current flow (parser.DiffFlows, viewer access).
// @Summary      Diff version vs. current
// @Description  handleDiffFlowVersion returns the structural diff between a historical version and the current flow content. Read-only (viewer access). Uses parser.DiffFlows on the two unmarshaled FlowDocuments.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Diff"
// @Router       /api/library/{id}/versions/{vn}/diff [get]
func (h *LibraryHandler) handleDiffFlowVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	historical, err := h.libSvc.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	oldDoc, err := unmarshalFlowContent(historical.Content)
	if err != nil {
		render.Error(w, fmt.Errorf("parse historical version: %w", err), http.StatusInternalServerError)
		return
	}
	newDoc, err := unmarshalFlowContent(current.Content)
	if err != nil {
		render.Error(w, fmt.Errorf("parse current flow: %w", err), http.StatusInternalServerError)
		return
	}
	// DiffFlows(old, new): + = added since the snapshot, - = removed since it.
	render.JSON(w, parser.DiffFlows(oldDoc, newDoc))
}

// unmarshalFlowContent deserializes a stored flow Content (a serialized
// models.FlowDocument) into the parser's FlowDocument for diffing.
func unmarshalFlowContent(content json.RawMessage) (*models.FlowDocument, error) {
	if len(content) == 0 {
		return &models.FlowDocument{}, nil
	}
	var doc models.FlowDocument
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	// Stored content is client-supplied: never trust its desktop file path
	// (see resolveStoredFlow in internal/service/document_provider.go).
	doc.FilePath = ""
	return &doc, nil
}

// registerVersionRoutes wires the library flow-version sub-routes. Reading a
// version, and the two-blob diff, stream stored blobs after the authz check,
// outside the request transaction; save and restore keep it for atomicity.
// Listing and reading a version need FlowRead on the flow in the path, checked
// before the handler runs; save, restore and diff authorize the flow they load.
// Versions live in the server-side store: without one the list is empty and
// the rest answer 503.
func (h *LibraryHandler) registerVersionRoutes(rb *route.Builder) {
	read := requireLibraryFlow(h.libSvc, service.FlowRead, "id")
	rb.Route("/versions", func(rb *route.Builder) {
		rb.With(read).Get("/", h.handleListFlowVersions)
		rb.Post("/", h.handleSaveFlowVersion, route.Needs(capability.ServerStore), route.Idempotent())
		rb.With(read).Get("/{vn}", h.handleGetFlowVersion, route.NoTx(), route.Needs(capability.ServerStore))
		rb.Post("/{vn}/restore", h.handleRestoreFlowVersion, route.Needs(capability.ServerStore))
		rb.Get("/{vn}/diff", h.handleDiffFlowVersion, route.NoTx(), route.Needs(capability.ServerStore))
	})
}
