package api

import (
	"context"
	"errors"
	"time"

	"pad-analyzer/internal/platform/apperr"
)

// The caller's thread index (R-frontend-c26 (c)): a conversation belongs to the user whose turn or save created it
// (R-security-3), so the server can list a user's threads on a flow and keep their names, and any browser can rebuild
// its thread list from them. Both endpoints need chat access to the flow (viewer, chat scope), as streaming does.

// chatThreadsOrder names the thread list's keyset order in its cursors: a cursor is honoured only by this list, for
// the flow it was issued for.
const chatThreadsOrder = "chat_threads_updated_desc"

// ChatThreadsRequest is POST /api/chat/threads: the flow, and the page (limit 1-200, default 50; cursor: the previous
// page's nextCursor).
type ChatThreadsRequest struct {
	FlowID string `json:"flowId"`
	PageRequest
}

// FlowRef names the flow the list is of.
func (r ChatThreadsRequest) FlowRef() string { return r.FlowID }

// Validate requires the flow.
func (r ChatThreadsRequest) Validate() error {
	if r.FlowID == "" {
		return errors.New("flowId is required")
	}
	return nil
}

// ChatThread is one entry of the caller's thread index on a flow. ID is the conversation key: the thread id the
// client sends as conversationId when it streams, and as provider on get, save and clear. Title is the name the
// caller gave it ("" when unnamed: show the client's untitled label). CreatedAt and UpdatedAt are when the thread was
// stored first and when its messages last changed; MessageCount is how many messages it holds; Version is what a
// save or clear holds (baseVersion), as GET returns it.
type ChatThread struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	MessageCount int       `json:"messageCount"`
	Version      int64     `json:"version"`
}

// @Summary      List the caller's chat threads on a flow
// @Description  The caller's stored chat threads (conversations) on a flow, newest change first (updatedAt, then id, both descending), for rebuilding the client's thread list in any browser. Only the caller's own threads: another user's are never listed, and neither are conversations stored before conversations had owners (they belong to nobody; a client keeps the ones it already lists). A page holds at most limit items (default 50, at most 200); nextCursor, present while more follow, is passed back as cursor for the next page, with the same flowId (another flow's list refuses it: 400). Needs chat access to the flow (viewer; scope chat): 404 for a missing flow, 403 without access. Where conversations are not stored server-side (the desktop app, a deployment without accounts) the list is empty.
// @Tags         chat
// @Param        request body ChatThreadsRequest true "flowId, limit, cursor"
// @Accept       json
// @Produce      json
// @Success      200 {object} api.Page[api.ChatThread] "OK"
// @Failure      400 {object} render.ErrorResponse "No flowId, or a cursor this list did not issue"
// @Failure      403 {object} render.ErrorResponse "No chat access to the flow"
// @Failure      404 {object} render.ErrorResponse "No such flow"
// @Router       /api/chat/threads [post]
func (h *ChatHandler) listThreads(ctx context.Context, req ChatThreadsRequest) (Page[ChatThread], error) {
	filters := listFilterDigest(req.FlowID)
	after, err := decodeListCursor(req.Cursor, chatThreadsOrder, filters)
	if err != nil {
		return Page[ChatThread]{}, apperr.Invalid("", err.Error()).Wrap(err)
	}
	// One more than the page: whether another page follows, without counting.
	threads, err := h.chatSvc.ListThreads(ctx, req.FlowID, after, req.Limit+1)
	if err != nil {
		return Page[ChatThread]{}, err
	}
	page := Page[ChatThread]{Items: make([]ChatThread, 0, min(len(threads), req.Limit))}
	for i, t := range threads {
		if i == req.Limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeListCursor(chatThreadsOrder, filters, last.UpdatedAt, last.ID)
			break
		}
		page.Items = append(page.Items, ChatThread{
			ID: t.Scope, Title: t.Title, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			MessageCount: t.MessageCount, Version: t.Version,
		})
	}
	return page, nil
}

// maxThreadKeyBytes bounds a conversation key a title names: a thread id is a 36-byte uuid, a legacy key "flow" or a
// block id.
const maxThreadKeyBytes = 256

// maxThreadTitleBytes bounds the title as sent; the stored title is clamped further (200 characters).
const maxThreadTitleBytes = 4096

// ChatThreadTitleRequest is POST /api/chat/title: the caller names one of their threads.
type ChatThreadTitleRequest struct {
	FlowID string `json:"flowId"`
	// ConversationID is the thread's conversation key (ChatThread.ID).
	ConversationID string `json:"conversationId"`
	// Title is the name; "" removes it.
	Title string `json:"title"`
}

// FlowRef names the thread's flow.
func (r ChatThreadTitleRequest) FlowRef() string { return r.FlowID }

// Validate requires the flow and the key, and bounds the sizes.
func (r ChatThreadTitleRequest) Validate() error {
	switch {
	case r.FlowID == "":
		return errors.New("flowId is required")
	case r.ConversationID == "":
		return errors.New("conversationId is required")
	case len(r.ConversationID) > maxThreadKeyBytes:
		return errors.New("conversationId is too long")
	case len(r.Title) > maxThreadTitleBytes:
		return errors.New("title is too long")
	}
	return nil
}

// ChatThreadTitleResponse is the title answer: status, and the conversation's version after naming where the store has
// versions — unchanged for a stored thread; for a thread not stored yet, the version of the empty conversation the
// name created (a client that held 0 and has stored nothing adopts it as its baseVersion).
type ChatThreadTitleResponse struct {
	Status  string `json:"status"`
	Version *int64 `json:"version,omitempty"`
}

// @Summary      Name a chat thread
// @Description  Sets the title of one of the caller's chat threads on a flow ("" removes it), so every browser's thread list shows it (POST /api/chat/threads). With "Save conversation history" off the title is accepted and dropped (no version). A thread not stored yet is created only under a thread id (a lower-case uuid, as the client generates): under any other key ("flow", a block id) a missing conversation is not created — the title is accepted and dropped (no version) — and neither is a thread the caller cleared in the last minutes (a rename racing the close). A conversation stored before owners under a thread id becomes the caller's. The title is stored clamped — NFC, control, bidi and zero-width characters dropped, whitespace collapsed, at most 200 characters — and as its scrubbed copy (secrets masked [REDACTED]), like the messages. A thread not stored yet (named before its first turn) is stored empty with the name, at a version of its own, which the answer carries (version: the conversation's version after naming, unchanged for a stored thread); a client that held 0 and has stored nothing adopts it as its baseVersion, else its next save at 0 gets 409 CONVERSATION_CONFLICT and reloads as usual. Naming changes neither the version of a stored thread nor updatedAt. A key under which another user's conversation is stored is refused with 409 CONVERSATION_KEY_TAKEN, a shared conversation stored before owners (under "flow" or a block id) with 403 CONVERSATION_UNOWNED (it belongs to nobody; keep the name in the client). Needs chat access to the flow (viewer; scope chat). Where conversations are not stored server-side (the desktop app, a deployment without accounts) the title is accepted and dropped.
// @Tags         chat
// @Param        request body ChatThreadTitleRequest true "flowId, conversationId, title"
// @Accept       json
// @Produce      json
// @Success      200 {object} api.ChatThreadTitleResponse "OK"
// @Failure      400 {object} render.ErrorResponse "No flowId or conversationId, or a field over its size"
// @Failure      403 {object} render.ErrorResponse "No chat access to the flow, or CONVERSATION_UNOWNED"
// @Failure      404 {object} render.ErrorResponse "No such flow"
// @Failure      409 {object} render.ErrorResponse "CONVERSATION_KEY_TAKEN: the key holds another user's conversation"
// @Router       /api/chat/title [post]
func (h *ChatHandler) setThreadTitle(ctx context.Context, req ChatThreadTitleRequest) (ChatThreadTitleResponse, error) {
	saved, err := h.chatSvc.SetThreadTitle(ctx, req.FlowID, req.ConversationID, req.Title)
	if err != nil {
		return ChatThreadTitleResponse{}, err
	}
	resp := ChatThreadTitleResponse{Status: "ok"}
	if saved.Versioned {
		resp.Version = &saved.Version
	}
	return resp, nil
}
