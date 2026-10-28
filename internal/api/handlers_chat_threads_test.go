package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// R-security-3 and R-frontend-c26 (c) at the API: owner-scoped clear for viewers, the thread list and the title.

const ownerFlowID = "f-owner"

// chatOwnerRouter is a JWT router whose chat service stores conversations in the (fake) cloud store, with one flow
// owned by "owner" that "editor" (editor) and "viewer" (viewer) collaborate on, and an "outsider".
func chatOwnerRouter(t *testing.T, backend storageif.StorageBackend) (*Router, map[string]string) {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	rt := newTestRouter(backend, true)
	h := moduleOf[*ChatHandler](t, rt)
	notifier := &mockNotifier{}
	settings := storage.NewInMemorySettingsStore()
	analysis, err := service.NewAnalysisService(notifier, settings, nil, service.AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "test", nil }, nil, nil, nil)
	h.chatSvc = service.NewChatService(notifier, t.TempDir(), h.flowSvc, analysis, settings, factory, backend, config.ModeCloud, service.ChatOptions{})
	ctx := context.Background()
	bearers := map[string]string{}
	for _, id := range []string{"owner", "editor", "viewer", "outsider"} {
		if err := backend.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		bearers[id] = sessionBearer(t, rt, id, auth.RoleMember)
	}
	if err := backend.SaveFlow(ctx, &storageif.FlowDocument{ID: ownerFlowID, Name: "t", OwnerID: "owner", Content: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	for id, perm := range map[string]string{"editor": "editor", "viewer": "viewer"} {
		if err := backend.AddCollaborator(ctx, ownerFlowID, &storageif.Collaborator{UserID: id, Permission: perm}); err != nil {
			t.Fatalf("AddCollaborator %s: %v", id, err)
		}
	}
	return rt, bearers
}

// seedThread stores owner's conversation under key, as their turn would.
func seedThread(t *testing.T, b storageif.ConversationStore, owner, key string) int64 {
	t.Helper()
	v, err := b.SaveConversationIf(context.Background(), ownerFlowID, key, owner, 0, []storageif.ChatMessage{
		{ID: "m-" + owner, Role: "user", Content: owner + "'s question", Timestamp: "2026-10-27T10:00:00Z"},
	})
	if err != nil {
		t.Fatalf("seed %s's thread: %v", owner, err)
	}
	return v
}

func stored(t *testing.T, b storageif.ConversationStore, key string) int {
	t.Helper()
	msgs, _, err := b.LoadConversationVersioned(context.Background(), ownerFlowID, key, "")
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

// The viewer gap: a viewer (ChatUse) clears their own thread, with and without baseVersion; nobody clears another
// user's thread (the clear is the no-op of a missing conversation, and the thread stays).
func TestChatClear_ViewerClearsOwnThreadOnly(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt, bearer := chatOwnerRouter(t, backend)
	const mine, blind, theirs = "11111111-0000-4000-8000-000000000001", "11111111-0000-4000-8000-000000000002", "11111111-0000-4000-8000-000000000003"
	v := seedThread(t, backend, "viewer", mine)
	seedThread(t, backend, "viewer", blind)
	seedThread(t, backend, "editor", theirs)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "provider": mine, "baseVersion": v})
	checkStatus(t, rr, http.StatusOK)
	if n := stored(t, backend, mine); n != 0 {
		t.Errorf("the viewer's versioned clear left %d messages", n)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "provider": blind})
	checkStatus(t, rr, http.StatusOK)
	if n := stored(t, backend, blind); n != 0 {
		t.Errorf("the viewer's blind clear left %d messages", n)
	}
	// The viewer, and the flow's owner, clearing the editor's thread: nothing happens to it.
	for _, who := range []string{"viewer", "owner"} {
		rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer[who], map[string]any{"flowId": ownerFlowID, "provider": theirs})
		checkStatus(t, rr, http.StatusOK)
		if n := stored(t, backend, theirs); n != 1 {
			t.Fatalf("%s's clear removed the editor's thread", who)
		}
	}
	// Outside the flow: refused before anything is looked up.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer["outsider"], map[string]any{"flowId": ownerFlowID, "provider": theirs})
	checkStatus(t, rr, http.StatusForbidden)
}

// An unowned (pre-owner) conversation is cleared only by someone who may delete the flow: an editor gets 403
// CONVERSATION_UNOWNED and the conversation stays; the flow's owner clears it.
func TestChatClear_UnownedNeedsTheFlowDeleteRight(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt, bearer := chatOwnerRouter(t, backend)
	if err := backend.SaveConversation(context.Background(), ownerFlowID, "flow", []storageif.ChatMessage{{ID: "l", Role: "user", Content: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"editor", "viewer"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer[who], map[string]any{"flowId": ownerFlowID, "provider": "flow"})
		checkStatus(t, rr, http.StatusForbidden)
		if code := errorCode(t, rr); code != service.CodeConversationUnowned {
			t.Errorf("%s: code = %q, want %s", who, code, service.CodeConversationUnowned)
		}
	}
	if n := stored(t, backend, "flow"); n != 1 {
		t.Fatal("a refused clear removed the unowned conversation")
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer["owner"], map[string]any{"flowId": ownerFlowID, "provider": "flow"})
	checkStatus(t, rr, http.StatusOK)
	if n := stored(t, backend, "flow"); n != 0 {
		t.Error("the flow owner's clear left the unowned conversation")
	}
}

// chatThreadsPage is the list's wire shape.
type chatThreadsPage struct {
	Items []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		CreatedAt    string `json:"createdAt"`
		UpdatedAt    string `json:"updatedAt"`
		MessageCount int    `json:"messageCount"`
		Version      int64  `json:"version"`
	} `json:"items"`
	NextCursor string `json:"nextCursor"`
}

// POST /api/chat/threads is the caller's thread index on a flow: their threads only, newest first, paged by cursor;
// a title set through POST /api/chat/title is in it. A viewer may use both (ChatUse); an outsider may not.
func TestChatThreads_ListAndTitle(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt, bearer := chatOwnerRouter(t, backend)
	keys := []string{"22222222-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002", "22222222-0000-4000-8000-000000000003"}
	versions := map[string]int64{}
	for _, k := range keys {
		versions[k] = seedThread(t, backend, "viewer", k)
	}
	seedThread(t, backend, "editor", "22222222-0000-4000-8000-0000000000ee")
	if err := backend.SaveConversation(context.Background(), ownerFlowID, "flow", []storageif.ChatMessage{{ID: "l", Role: "user", Content: "legacy"}}); err != nil {
		t.Fatal(err)
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/title", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "conversationId": keys[0], "title": "  Budget\tplan  "})
	checkStatus(t, rr, http.StatusOK)

	var all chatThreadsPage
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["viewer"], map[string]any{"flowId": ownerFlowID})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &all)
	if len(all.Items) != 3 || all.NextCursor != "" {
		t.Fatalf("viewer's threads = %+v, want the three seeded, one page", all)
	}
	for _, it := range all.Items {
		if it.MessageCount != 1 || it.Version != versions[it.ID] || it.CreatedAt == "" || it.UpdatedAt == "" {
			t.Errorf("thread %+v: want 1 message at version %d with both times", it, versions[it.ID])
		}
		if want := map[bool]string{true: "Budget plan", false: ""}[it.ID == keys[0]]; it.Title != want {
			t.Errorf("thread %s title = %q, want %q", it.ID, it.Title, want)
		}
	}

	// Paged: two, then the rest after the cursor.
	var first, rest chatThreadsPage
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "limit": 2})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &first)
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, want two and a cursor", first)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "limit": 2, "cursor": first.NextCursor})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &rest)
	if len(rest.Items) != 1 || rest.NextCursor != "" {
		t.Fatalf("second page = %+v, want the last one, no cursor", rest)
	}
	got := []string{first.Items[0].ID, first.Items[1].ID, rest.Items[0].ID}
	for i, it := range all.Items {
		if got[i] != it.ID {
			t.Errorf("paged order %v differs from the single page's at %d", got, i)
		}
	}
	// A cursor is the list's own: another flow's list refuses it.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["owner"], map[string]any{"flowId": ownerFlowID, "cursor": "bm90LWEtY3Vyc29y"})
	checkStatus(t, rr, http.StatusBadRequest)

	// The editor sees only their own; the owner has none (the unowned "flow" conversation is nobody's).
	var editor, owner chatThreadsPage
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["editor"], map[string]any{"flowId": ownerFlowID})
	decodeJSON(t, rr, &editor)
	if len(editor.Items) != 1 || editor.Items[0].ID != "22222222-0000-4000-8000-0000000000ee" {
		t.Errorf("editor's threads = %+v, want only their own", editor.Items)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["owner"], map[string]any{"flowId": ownerFlowID})
	decodeJSON(t, rr, &owner)
	if len(owner.Items) != 0 {
		t.Errorf("owner's threads = %+v, want none", owner.Items)
	}

	// Naming another user's thread: 409 CONVERSATION_KEY_TAKEN; an unowned one: 403 CONVERSATION_UNOWNED.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/title", bearer["editor"], map[string]any{"flowId": ownerFlowID, "conversationId": keys[1], "title": "mine"})
	checkStatus(t, rr, http.StatusConflict)
	if code := errorCode(t, rr); code != service.CodeConversationKeyTaken {
		t.Errorf("naming another's thread: code = %q, want %s", code, service.CodeConversationKeyTaken)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/title", bearer["owner"], map[string]any{"flowId": ownerFlowID, "conversationId": "flow", "title": "x"})
	checkStatus(t, rr, http.StatusForbidden)

	// Outsiders: 403 on both; a missing flow: 404; a missing flowId: 400.
	for _, path := range []string{"/api/chat/threads", "/api/chat/title"} {
		rr = doRequestWithAuth(t, rt, http.MethodPost, path, bearer["outsider"], map[string]any{"flowId": ownerFlowID, "conversationId": keys[0], "title": "x"})
		if path == "/api/chat/threads" {
			rr = doRequestWithAuth(t, rt, http.MethodPost, path, bearer["outsider"], map[string]any{"flowId": ownerFlowID})
		}
		checkStatus(t, rr, http.StatusForbidden)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["viewer"], map[string]any{"flowId": "no-such-flow"})
	checkStatus(t, rr, http.StatusNotFound)
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["viewer"], map[string]any{})
	checkStatus(t, rr, http.StatusBadRequest)
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/title", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "title": "no key"})
	checkStatus(t, rr, http.StatusBadRequest)
}

// A save on another user's key is 409 CONVERSATION_KEY_TAKEN, never the CONVERSATION_CONFLICT a client answers by
// reloading and saving again.
func TestChatSave_AnotherUsersKeyIsKeyTaken(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt, bearer := chatOwnerRouter(t, backend)
	const key = "33333333-0000-4000-8000-000000000001"
	seedThread(t, backend, "viewer", key)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/save", bearer["editor"], map[string]any{
		"flowId": ownerFlowID, "provider": key, "baseVersion": 0,
		"messages": []any{map[string]any{"id": "e1", "role": "user", "content": "editor was here", "timestamp": "2026-10-27T10:00:00Z"}},
	})
	checkStatus(t, rr, http.StatusConflict)
	if code := errorCode(t, rr); code != service.CodeConversationKeyTaken {
		t.Errorf("code = %q, want %s", code, service.CodeConversationKeyTaken)
	}
	if n := stored(t, backend, key); n != 1 {
		t.Errorf("the refused save changed the viewer's thread (%d messages)", n)
	}
}

// A shared conversation (stored before owners under "flow") keeps its list against a client's save unless the caller
// may delete the flow; a title under a key that is not a thread id stores nothing (R-security-3).
func TestChatSave_SharedConversationNeedsTheFlowDeleteRight(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt, bearer := chatOwnerRouter(t, backend)
	if err := backend.SaveConversation(context.Background(), ownerFlowID, "flow", []storageif.ChatMessage{{ID: "l", Role: "user", Content: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	save := func(who string) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/save", bearer[who], map[string]any{
			"flowId": ownerFlowID, "provider": "flow",
			"messages": []any{map[string]any{"id": who, "role": "user", "content": who + " replaced it", "timestamp": "2026-10-27T10:00:00Z"}},
		})
	}
	rr := save("editor")
	checkStatus(t, rr, http.StatusForbidden)
	if code := errorCode(t, rr); code != service.CodeConversationUnowned {
		t.Errorf("editor's save: code = %q, want %s", code, service.CodeConversationUnowned)
	}
	if msgs, _, _ := backend.LoadConversationVersioned(context.Background(), ownerFlowID, "flow", ""); len(msgs) != 1 || msgs[0].ID != "l" {
		t.Fatalf("the refused save changed the shared conversation: %+v", msgs)
	}
	checkStatus(t, save("owner"), http.StatusOK)

	var body struct {
		Status  string `json:"status"`
		Version *int64 `json:"version"`
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/title", bearer["viewer"], map[string]any{"flowId": ownerFlowID, "conversationId": "block-or-flow-key", "title": "squat"})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &body)
	if body.Version != nil || stored(t, backend, "block-or-flow-key") != 0 {
		t.Errorf("a title under a non-thread key = %+v; want accepted, dropped, no version", body)
	}
}
