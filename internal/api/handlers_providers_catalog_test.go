package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// TestProviderCatalogRoute_ListsEveryProviderOfTheProfile: GET /api/providers/catalog answers {providers: [...]}
// — every registered provider of the running profile in display order, configured or not — with the wire
// shape the generic settings panel renders from.
func TestProviderCatalogRoute_ListsEveryProviderOfTheProfile(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/providers/catalog", nil)
	checkStatus(t, rr, http.StatusOK)
	var body struct {
		Providers []map[string]any `json:"providers"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Providers) == 0 {
		t.Fatal("empty catalogue")
	}
	for i, p := range body.Providers {
		for _, k := range []string{"id", "name", "authType", "modes", "caps", "credential", "endpoints", "configured"} {
			if _, ok := p[k]; !ok {
				t.Errorf("entry %d (%v) lacks %q", i, p["id"], k)
			}
		}
		cred, _ := p["credential"].(map[string]any)
		if _, ok := cred["fields"].([]any); !ok {
			t.Errorf("%v: credential.fields is %T, want an array (possibly empty)", p["id"], cred["fields"])
		}
		if spec, ok := ai.SpecOf(p["id"].(string)); !ok {
			t.Errorf("%v: not a registered provider", p["id"])
		} else if ai.Specs()[i].ID != spec.ID {
			t.Errorf("entry %d is %s, want display order", i, spec.ID)
		}
		if p["configured"] != false {
			t.Errorf("%v: configured = %v on a router with no stored keys", p["id"], p["configured"])
		}
	}
	// The anonymous caller is refused like /api/providers/list.
	rr = doRequestWithAuth(t, newTestRouter(testutil.NewFakeBackend(), true), http.MethodGet, "/api/providers/catalog", "", nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

// TestProviderUnknown_CodeOnKeysTestAndChat: an id the registry does not know gets the stable PROVIDER_UNKNOWN
// code everywhere a client sends one — /api/keys/*, the connection test (its 200 shape, ok:false) and a chat turn
// (refused before any stream starts), so a typo or a stale client is told apart from an outage.
func TestProviderUnknown_CodeOnKeysTestAndChat(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), false)
	for _, path := range []string{"/api/keys/save", "/api/keys/has", "/api/keys/delete"} {
		rr := doRequest(t, rt, http.MethodPost, path, map[string]any{"provider": "nope", "key": "k"})
		checkStatus(t, rr, http.StatusBadRequest)
		if got := azureDecode(t, rr); got["code"] != service.ProviderCodeUnknown {
			t.Errorf("%s: response = %v, want code %s", path, got, service.ProviderCodeUnknown)
		}
	}
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/test", map[string]any{"provider": "nope"})
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["ok"] != false || got["code"] != service.ProviderCodeUnknown {
		t.Errorf("/api/providers/test: response = %v, want ok:false code %s", got, service.ProviderCodeUnknown)
	}

	// A chat turn resolves its flow from the store with accounts on (local mode chats about the loaded flow).
	jwt := newJWTTestRouter(t)
	seedFlow(t, jwt, "flow-a", "alice")
	tok := jwtBearer(t, jwt, "alice", "alice@example.com")
	for _, path := range []string{"/api/chat/stream", "/api/chat/preview-context"} {
		rr = doRequestWithAuth(t, jwt, http.MethodPost, path, tok, map[string]any{
			"flowId": "flow-a", "provider": "nope", "model": "m", "userMessage": "hi",
			"clientStreamId": "11111111-1111-4111-8111-111111111111", "conversationId": "22222222-2222-4222-8222-222222222222",
		})
		checkStatus(t, rr, http.StatusBadRequest)
		if got := azureDecode(t, rr); got["code"] != service.ProviderCodeUnknown {
			t.Errorf("%s: response = %v, want code %s", path, got, service.ProviderCodeUnknown)
		}
	}
}
