package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// versionedChatBody is the part of the GET/save responses the version tests read.
type versionedChatBody struct {
	Status   string            `json:"status"`
	Version  *int64            `json:"version"`
	Messages []json.RawMessage `json:"messages"`
}

func chatVersionRouter(t *testing.T) (rt *Router, auth string) {
	t.Helper()
	// The router's settings store reads the OS config dir: point it at a temp dir so "Save conversation history"
	// has its default (on) and the real settings file is never read.
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	backend := testutil.NewFakeBackend()
	rt = newTestRouter(backend, true)
	// The test router runs the chat service on files; versions belong to the cloud store.
	h := moduleOf[*ChatHandler](t, rt)
	notifier := &mockNotifier{}
	settings := storage.NewInMemorySettingsStore()
	analysis, err := service.NewAnalysisService(notifier, settings, nil, service.AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "test", nil }, nil, nil, nil)
	h.chatSvc = service.NewChatService(notifier, t.TempDir(), h.flowSvc, analysis, settings, factory, backend, config.ModeCloud, service.ChatOptions{})
	doc := &interfaces.FlowDocument{ID: "f-cv", Name: "t", OwnerID: "u1"}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	return rt, jwtBearer(t, rt, "u1", "u1@example.com")
}

func chatMsg(id, content string) map[string]any {
	return map[string]any{"id": id, "role": "user", "content": content, "timestamp": "2026-10-13T10:00:00Z"}
}

func chatPost(t *testing.T, rt *Router, auth, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body["flowId"] = "f-cv"
	body["provider"] = "11111111-2222-4333-8444-555555555555"
	return doRequestWithAuth(t, rt, http.MethodPost, path, auth, body)
}

// The version GET reports is the one a save is compared against; a save that carries a stale one is refused with
// the conflict code and the current version, and changes nothing.
func TestChatSave_BaseVersionCompareAndSwap(t *testing.T) {
	rt, auth := chatVersionRouter(t)

	// A conversation that does not exist yet is at version 0, and 0 creates it.
	rr := chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	var got versionedChatBody
	decodeJSON(t, rr, &got)
	if got.Version == nil || *got.Version != 0 {
		t.Fatalf("GET of a new conversation: version = %v, want 0", got.Version)
	}

	rr = chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": 0, "messages": []any{chatMsg("m1", "one")}})
	checkStatus(t, rr, http.StatusOK)
	var saved versionedChatBody
	decodeJSON(t, rr, &saved)
	if saved.Status != "ok" || saved.Version == nil || *saved.Version <= 0 {
		t.Fatalf("save at base 0: %s, want ok with a positive version", rr.Body.String())
	}
	v1 := *saved.Version

	rr = chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	got = versionedChatBody{}
	decodeJSON(t, rr, &got)
	if got.Version == nil || *got.Version != v1 || len(got.Messages) != 1 {
		t.Fatalf("GET after the save: %s, want version %d with 1 message", rr.Body.String(), v1)
	}

	// The next save moves the version; a save still holding v1 conflicts.
	rr = chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": v1, "messages": []any{chatMsg("m1", "one"), chatMsg("m2", "two")}})
	checkStatus(t, rr, http.StatusOK)
	saved = versionedChatBody{}
	decodeJSON(t, rr, &saved)
	v2 := *saved.Version
	if v2 <= v1 {
		t.Fatalf("version after a change = %d, want more than %d", v2, v1)
	}

	rr = chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": v1, "messages": []any{chatMsg("m9", "stale")}})
	checkStatus(t, rr, http.StatusConflict)
	var env render.ErrorResponse
	decodeJSON(t, rr, &env)
	if env.Code != "CONVERSATION_CONFLICT" {
		t.Errorf("code = %q, want CONVERSATION_CONFLICT", env.Code)
	}
	if env.Details["version"] != float64(v2) {
		t.Errorf("details.version = %v, want the current %d", env.Details["version"], v2)
	}
	rr = chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	got = versionedChatBody{}
	decodeJSON(t, rr, &got)
	if len(got.Messages) != 2 || got.Version == nil || *got.Version != v2 {
		t.Errorf("the refused save changed the conversation: %s", rr.Body.String())
	}
}

// Clients from before versions send no baseVersion: their save is still accepted as the full replace it was, and it
// moves the version so a client holding the old one finds out.
func TestChatSave_WithoutBaseVersionStaysABlindSave(t *testing.T) {
	rt, auth := chatVersionRouter(t)
	rr := chatPost(t, rt, auth, "/api/chat/save", map[string]any{"messages": []any{chatMsg("m1", "one")}})
	checkStatus(t, rr, http.StatusOK)
	var saved versionedChatBody
	decodeJSON(t, rr, &saved)
	if saved.Status != "ok" || saved.Version != nil {
		t.Errorf("blind save answered %s, want {status: ok} without a version", rr.Body.String())
	}

	rr = chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	var got versionedChatBody
	decodeJSON(t, rr, &got)
	held := *got.Version

	rr = chatPost(t, rt, auth, "/api/chat/save", map[string]any{"messages": []any{chatMsg("m1", "one"), chatMsg("m2", "two")}})
	checkStatus(t, rr, http.StatusOK)
	rr = chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": held, "messages": []any{chatMsg("m3", "late")}})
	checkStatus(t, rr, http.StatusConflict)
}

func TestChatSave_NegativeBaseVersionIsRefused(t *testing.T) {
	rt, auth := chatVersionRouter(t)
	rr := chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": -1, "messages": []any{chatMsg("m1", "one")}})
	checkStatus(t, rr, http.StatusBadRequest)
	rr = chatPost(t, rt, auth, "/api/chat/clear", map[string]any{"baseVersion": -3})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestChatClear_BaseVersion(t *testing.T) {
	rt, auth := chatVersionRouter(t)
	rr := chatPost(t, rt, auth, "/api/chat/save", map[string]any{"baseVersion": 0, "messages": []any{chatMsg("m1", "one")}})
	checkStatus(t, rr, http.StatusOK)
	var saved versionedChatBody
	decodeJSON(t, rr, &saved)
	v := *saved.Version

	rr = chatPost(t, rt, auth, "/api/chat/clear", map[string]any{"baseVersion": v + 1000})
	checkStatus(t, rr, http.StatusConflict)
	var env render.ErrorResponse
	decodeJSON(t, rr, &env)
	if env.Code != "CONVERSATION_CONFLICT" || env.Details["version"] != float64(v) {
		t.Errorf("stale clear = %s, want CONVERSATION_CONFLICT carrying version %d", rr.Body.String(), v)
	}

	rr = chatPost(t, rt, auth, "/api/chat/clear", map[string]any{"baseVersion": v})
	checkStatus(t, rr, http.StatusOK)
	rr = chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	var got versionedChatBody
	decodeJSON(t, rr, &got)
	if len(got.Messages) != 0 || got.Version == nil || *got.Version != 0 {
		t.Errorf("after the clear: %s, want an empty conversation at version 0", rr.Body.String())
	}

	// No baseVersion: the unconditional delete it was.
	chatPost(t, rt, auth, "/api/chat/save", map[string]any{"messages": []any{chatMsg("m1", "one")}})
	rr = chatPost(t, rt, auth, "/api/chat/clear", map[string]any{})
	checkStatus(t, rr, http.StatusOK)
}
