package api

import (
	"errors"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// handleRemoveBlock deletes one block (with descendants) from the source —
// parse-gated + snapshotted server-side (undo via the snapshot endpoints).
// @Summary      Remove a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/remove [post]
func (h *FlowHandler) handleRemoveBlock(w http.ResponseWriter, r *http.Request) {
	h.handleBlockEdit(w, r, "remove")
}

// @Summary      Duplicate a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/duplicate [post]
func (h *FlowHandler) handleDuplicateBlock(w http.ResponseWriter, r *http.Request) {
	h.handleBlockEdit(w, r, "duplicate")
}

func (h *FlowHandler) handleBlockEdit(w http.ResponseWriter, r *http.Request, kind string) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" {
		render.Error(w, fmt.Errorf("blockId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_" + kind)
	var updated *models.FlowDocument
	var err error
	if kind == "remove" {
		updated, err = h.flowSvc.RemoveBlock(r.Context(), doc, req.BlockID)
	} else {
		updated, err = h.flowSvc.DuplicateBlock(r.Context(), doc, req.BlockID)
	}
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // stale-file/guard errors carry their own guidance
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// @Summary      Update block properties
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/properties [post]
func (h *FlowHandler) handleUpdateBlockProperties(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string            `json:"flowId"`
		BlockID string            `json:"blockId"`
		Changes map[string]string `json:"changes"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || len(req.Changes) == 0 {
		render.Error(w, fmt.Errorf("blockId and at least one change are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_properties")
	updated, err := h.flowSvc.UpdateBlockProperties(r.Context(), doc, req.BlockID, req.Changes)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// @Summary      Move a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/move [post]
func (h *FlowHandler) handleMoveBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID    string `json:"flowId"`
		BlockID   string `json:"blockId"`
		Direction string `json:"direction"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" {
		render.Error(w, fmt.Errorf("blockId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_move")
	updated, err := h.flowSvc.MoveBlock(r.Context(), doc, req.BlockID, req.Direction)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// @Summary      Bulk delete blocks
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/remove-batch [post]
func (h *FlowHandler) handleRemoveBlocks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string   `json:"flowId"`
		BlockIDs []string `json:"blockIds"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.BlockIDs) == 0 {
		render.Error(w, fmt.Errorf("blockIds must not be empty"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_remove_batch")
	updated, err := h.flowSvc.RemoveBlocks(r.Context(), doc, req.BlockIDs)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleRenameBlock renames LABEL/COMMENT blocks, rewriting same-file GOTO
// references for labels. @Summary Rename a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document + gotoRefsUpdated"
// @Router       /api/flow/block/rename [post]
func (h *FlowHandler) handleRenameBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.Name == "" {
		render.Error(w, fmt.Errorf("blockId and name are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_rename")
	updated, gotoRefs, err := h.flowSvc.RenameBlock(r.Context(), doc, req.BlockID, req.Name)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	render.JSON(w, map[string]any{"document": updated, "gotoRefsUpdated": gotoRefs})
}

// handleMoveBlockTo reorders a block before/after a reference sibling (what
// drag-and-drop maps to). Same-scope only; re-parenting refused.
// @Summary      Move a block relative to another
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/move-to [post]
func (h *FlowHandler) handleMoveBlockTo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID     string `json:"flowId"`
		BlockID    string `json:"blockId"`
		RefBlockID string `json:"refBlockId"`
		Position   string `json:"position"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.RefBlockID == "" {
		render.Error(w, fmt.Errorf("blockId and refBlockId are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_move_to")
	updated, err := h.flowSvc.MoveBlockTo(r.Context(), doc, req.BlockID, req.RefBlockID, req.Position)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// registerBlockRoutes wires the /api/flow block-edit sub-routes.
func (h *FlowHandler) registerBlockRoutes(rb *route.Builder) {
	rb.Post("/block/remove", h.handleRemoveBlock)
	rb.Post("/block/duplicate", h.handleDuplicateBlock)
	rb.Post("/block/properties", h.handleUpdateBlockProperties)
	rb.Post("/block/move", h.handleMoveBlock)
	rb.Post("/block/move-to", h.handleMoveBlockTo)
	rb.Post("/block/remove-batch", h.handleRemoveBlocks)
	rb.Post("/block/rename", h.handleRenameBlock)
}
