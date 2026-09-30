package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Part 9, the ai side of the model registry: KeyStore keys, model ids,
// resource document validation and the registry provider.

func TestAzureConnectionKey_MapsTheFamily(t *testing.T) {
	cases := []struct {
		connID, key string
	}{
		{AzureDefaultConnectionID, "azure-foundry"},
		{"abc123def4", "azure-foundry/abc123def4"},
		{"0000000000", "azure-foundry/0000000000"},
	}
	for _, tc := range cases {
		if got := AzureConnectionKey(tc.connID); got != tc.key {
			t.Errorf("AzureConnectionKey(%q) = %q, want %q", tc.connID, got, tc.key)
		}
		if id, ok := ParseAzureConnectionKey(tc.key); !ok || id != tc.connID {
			t.Errorf("ParseAzureConnectionKey(%q) = %q, %v; want %q", tc.key, id, ok, tc.connID)
		}
		if !ValidAzureConnectionID(tc.connID) {
			t.Errorf("ValidAzureConnectionID(%q) = false", tc.connID)
		}
	}
	// Outside the family, or an id no resource can have: the default
	// resource's key is the bare family name, never "azure-foundry/default".
	for _, key := range []string{"", "azure-foundryX", "azure-foundry/", "azure-foundry/default", "azure-foundry/ABC123DEF4",
		"azure-foundry/abc123def", "azure-foundry/abc123def45", "azure-foundry/abc/123def4", "openai", "azure-foundry/../x123456"} {
		if id, ok := ParseAzureConnectionKey(key); ok {
			t.Errorf("ParseAzureConnectionKey(%q) = %q, want no resource", key, id)
		}
	}
	for _, id := range []string{"", "Default", "abc", "ABC123DEF4", "abc123def45", "abc/23def4", "../abcdefg"} {
		if ValidAzureConnectionID(id) {
			t.Errorf("ValidAzureConnectionID(%q) = true", id)
		}
	}
}

func TestRegistryModelID_Parse(t *testing.T) {
	if got := RegistryModelID("k3v9x2m1q0"); got != "m/k3v9x2m1q0" {
		t.Errorf("RegistryModelID = %q", got)
	}
	if id, ok := ParseRegistryModelID("m/k3v9x2m1q0"); !ok || id != "k3v9x2m1q0" {
		t.Errorf("ParseRegistryModelID = %q, %v", id, ok)
	}
	// Raw ids (a deployment, "claude:", "agent:") and malformed entry ids are
	// not registry ids.
	for _, id := range []string{"", "m/", "m/K3V9X2M1Q0", "m/k3v9x2m1q", "m/k3v9x2m1q00", "m/k3v9x2m1q-", "gpt-4o",
		"claude:sonnet", "agent:helper", "M/k3v9x2m1q0", " m/k3v9x2m1q0", "m/k3v9x2m1q0/"} {
		if got, ok := ParseRegistryModelID(id); ok {
			t.Errorf("ParseRegistryModelID(%q) = %q, want false", id, got)
		}
	}
}

func TestAzureModelTarget_PerKind(t *testing.T) {
	cases := []struct {
		kind, deployment, want string
		ok                     bool
	}{
		{AzureModelKindChat, "gpt-4o", "gpt-4o", true},
		{AzureModelKindClaude, "sonnet", "claude:sonnet", true},
		{AzureModelKindAgent, "Helper", "agent:Helper", true},
		{AzureModelKindEmbedding, "text-embedding-3-small", "", false},
		{"vision", "x", "", false},
		{AzureModelKindChat, "", "", false},
	}
	for _, tc := range cases {
		got, ok := AzureModelTarget(tc.kind, tc.deployment)
		if got != tc.want || ok != tc.ok {
			t.Errorf("AzureModelTarget(%q, %q) = %q, %v; want %q, %v", tc.kind, tc.deployment, got, ok, tc.want, tc.ok)
		}
		m := AzureRegisteredModel{EntryID: "abcdefghij", Kind: tc.kind, Deployment: tc.deployment}
		if got, ok := m.Target(); got != tc.want || ok != tc.ok {
			t.Errorf("%+v.Target() = %q, %v", m, got, ok)
		}
	}
	if got := (AzureRegisteredModel{EntryID: "abcdefghij"}).ModelID(); got != "m/abcdefghij" {
		t.Errorf("ModelID = %q", got)
	}
}

func TestValidateAzureRegisteredModel_Codes(t *testing.T) {
	cases := []struct {
		name string
		m    AzureRegisteredModel
		code string
	}{
		{"chat", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "gpt-4o"}, ""},
		{"claude with overrides", AzureRegisteredModel{Kind: AzureModelKindClaude, Deployment: "sonnet", ContextWindow: 200000, MaxOutputTokens: 64000}, ""},
		{"agent with space and letters", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "Review bot — Ünïcode"}, ""},
		{"embedding", AzureRegisteredModel{Kind: AzureModelKindEmbedding, Deployment: "text-embedding-3-small"}, ""},
		{"bounds", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "small", ContextWindow: AzureContextWindowMin, MaxOutputTokens: AzureMaxOutputTokensMin}, ""},
		{"unknown kind", AzureRegisteredModel{Kind: "vision", Deployment: "x"}, AzureCfgErrInvalidKind},
		{"no kind", AzureRegisteredModel{Deployment: "x"}, AzureCfgErrInvalidKind},
		{"deployment with a slash", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "a/b"}, AzureCfgErrInvalidDeploy},
		{"deployment with a space", AzureRegisteredModel{Kind: AzureModelKindClaude, Deployment: "a b"}, AzureCfgErrInvalidDeploy},
		{"deployment too long", AzureRegisteredModel{Kind: AzureModelKindEmbedding, Deployment: strings.Repeat("d", 65)}, AzureCfgErrInvalidDeploy},
		{"empty agent", AzureRegisteredModel{Kind: AzureModelKindAgent}, AzureCfgErrInvalidDeploy},
		{"agent too long", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: strings.Repeat("é", 129)}, AzureCfgErrInvalidDeploy},
		{"agent bidi control", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "evil\u202egnp"}, AzureCfgErrInvalidDeploy},
		{"agent zero-width", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "hel\u200blper"}, AzureCfgErrInvalidDeploy},
		{"agent NUL", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "a\x00b"}, AzureCfgErrInvalidDeploy},
		{"agent surrounding space", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: " helper"}, AzureCfgErrInvalidDeploy},
		{"window below", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "x", ContextWindow: 1023}, AzureCfgErrInvalidContextWindow},
		{"window above", AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "x", ContextWindow: 2_000_001}, AzureCfgErrInvalidContextWindow},
		{"output below", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "x", MaxOutputTokens: 255}, AzureCfgErrInvalidMaxOutput},
		{"output above", AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "x", MaxOutputTokens: 1_000_001}, AzureCfgErrInvalidMaxOutput},
		{"embedding window", AzureRegisteredModel{Kind: AzureModelKindEmbedding, Deployment: "e", ContextWindow: 8192}, AzureCfgErrInvalidContextWindow},
		{"embedding output", AzureRegisteredModel{Kind: AzureModelKindEmbedding, Deployment: "e", MaxOutputTokens: 1024}, AzureCfgErrInvalidMaxOutput},
	}
	for _, tc := range cases {
		err := ValidateAzureRegisteredModel(tc.m)
		if got := azureCfgCode(t, err); got != tc.code || (tc.code == "") != (err == nil) {
			t.Errorf("%s: ValidateAzureRegisteredModel = %v (code %q), want code %q", tc.name, err, got, tc.code)
		}
	}
}

// The resource document drops the single-document rule that it name a chat
// deployment (a resource may have no models yet), keeps every other rule, and
// can refuse the legacy deployment fields outright.
func TestValidateAzureConnectionConfig(t *testing.T) {
	const keyDoc = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"}}`
	cfg, err := ValidateAzureConnectionConfig(keyDoc, true)
	if err != nil || cfg.Endpoint != "https://my-res.openai.azure.com" {
		t.Fatalf("a resource without deployments = %+v, %v; want valid", cfg, err)
	}
	if code := azureCfgCode(t, ValidateAzureFoundryConfig(cfg)); code != AzureCfgErrMissingDeploy {
		t.Fatalf("the legacy save rule still applies to the legacy route: code %q", code)
	}

	legacy := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o","deployments":["a"],"claudeDeployments":["c"],"embeddingDeployment":"e"}`
	if _, err := ValidateAzureConnectionConfig(legacy, false); err != nil {
		t.Errorf("the default resource keeps its legacy fields: %v", err)
	}
	for _, field := range []string{`"chatDeployment":"x"`, `"deployments":["x"]`, `"claudeDeployments":["x"]`, `"embeddingDeployment":"x"`} {
		doc := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},` + field + `}`
		if _, err := ValidateAzureConnectionConfig(doc, true); azureCfgCode(t, err) != AzureCfgErrInvalidConfig {
			t.Errorf("%s on a non-default resource: %v, want INVALID_CONFIG", field, err)
		}
	}

	for name, tc := range map[string]struct {
		doc  string
		code string
	}{
		"empty":           {``, AzureCfgErrInvalidEndpoint},
		"not json":        {`{`, AzureCfgErrInvalidConfig},
		"unknown field":   {`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"name":"x"}`, AzureCfgErrInvalidConfig},
		"too large":       {`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"` + strings.Repeat("k", 2100) + `"}}`, AzureCfgErrTooLarge},
		"host":            {`{"endpoint":"https://evil.example.com","auth":{"mode":"api_key","key":"k"}}`, AzureCfgErrHostNotAllowed},
		"missing secret":  {`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key"}}`, AzureCfgErrMissingSecret},
		"project path":    {`{"endpoint":"https://my-res.services.ai.azure.com","projectEndpoint":"https://my-res.services.ai.azure.com/x","auth":{"mode":"api_key","key":"k"}}`, AzureCfgErrInvalidProject},
		"entra client id": {`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"entra","tenantId":"` + entraTestTenant + `","clientId":"nope","clientSecret":"s"}}`, AzureCfgErrInvalidAuth},
	} {
		if _, err := ValidateAzureConnectionConfig(tc.doc, true); azureCfgCode(t, err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
}

// registryTestProvider builds a registry provider for cfg on the server's
// client (validation bypassed: httptest hosts are loopback), with a fake
// Entra ID token source in Entra ID mode.
func registryTestProvider(t *testing.T, srv *azureTestServer, cfg AzureFoundryConfig, models []AzureRegisteredModel) *AzureFoundryProvider {
	t.Helper()
	var client *http.Client
	if srv != nil {
		client = srv.Client()
	}
	p := newAzureRegistryProvider(cfg, nil, models, "", client, processEntraSources)
	if p.configErr != nil {
		t.Fatalf("registry provider: %v", p.configErr)
	}
	if p.tokens != nil {
		p.tokens = newEntraTokenSource(&fakeCred{})
	}
	return p
}

// registryEntraConfig is an Entra ID + project document on the server.
func registryEntraConfig(srv *azureTestServer) AzureFoundryConfig {
	return AzureFoundryConfig{Endpoint: srv.URL, ProjectEndpoint: srv.URL + "/api/projects/p1",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: entraTestTenant, ClientID: azureSeamClientID, ClientSecret: "s"}}
}

func entry(id, kind, deployment string) AzureRegisteredModel {
	return AzureRegisteredModel{EntryID: id, Kind: kind, Deployment: deployment}
}

func modelIDs(models []ModelInfo) string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return strings.Join(ids, ",")
}

// The document's legacy deployment fields never reach a registry provider —
// they are cleared before validation, so a legacy document over the list
// caps still builds one — and its deployments are exactly the entries.
func TestAzureConnectionProvider_IgnoresLegacyFieldsAndCaps(t *testing.T) {
	ResetAzureFoundryCaches()
	legacy := make([]string, 17) // over azureMaxDeployments: Validate would refuse the document
	for i := range legacy {
		legacy[i] = fmt.Sprintf(`"legacy-%02d"`, i)
	}
	raw := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"legacy-chat",` +
		`"embeddingDeployment":"legacy-emb","claudeDeployments":["legacy-claude"],"deployments":[` + strings.Join(legacy, ",") + `]}`
	if _, err := ParseAzureFoundryConfig(raw); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// Well over both caps: 30 chat and 25 Claude entries.
	var models []AzureRegisteredModel
	for i := range 30 {
		models = append(models, entry(fmt.Sprintf("c%09d", i), AzureModelKindChat, fmt.Sprintf("chat-%02d", i)))
	}
	for i := range 25 {
		models = append(models, entry(fmt.Sprintf("k%09d", i), AzureModelKindClaude, fmt.Sprintf("claude-%02d", i)))
	}
	p := NewAzureFoundryConnectionProvider(raw, models, "").(*AzureFoundryProvider)
	if p.configErr != nil {
		t.Fatalf("configErr = %v, want none: the legacy fields and the caps do not apply", p.configErr)
	}
	cfg := p.Config()
	if cfg.ChatDeployment != "" || cfg.EmbeddingDeployment != "" || len(cfg.Deployments) != 30 || len(cfg.ClaudeDeployments) != 25 {
		t.Errorf("config = chat %q emb %q %d deployments %d claude, want the entries only", cfg.ChatDeployment, cfg.EmbeddingDeployment, len(cfg.Deployments), len(cfg.ClaudeDeployments))
	}
	listed, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 55 || strings.Contains(modelIDs(listed), "legacy") {
		t.Errorf("listing = %d entries (%s), want the 55 entries and nothing legacy", len(listed), modelIDs(listed))
	}
	if p.DefaultModel() != "" {
		t.Errorf("DefaultModel = %q, want none: the registry picks the default", p.DefaultModel())
	}
}

// The provider sorts the entry set itself, so the picker path and the chat
// path — both handed the resource's full set, in whatever order — build the
// same deployment lists and share one listing cache key. Invalid entries and
// out-of-range overrides are dropped; chat and Claude entries share one name
// space (the chat entry wins, as normalize does for the document lists).
func TestAzureConnectionProvider_SortedEntriesKeyOneListing(t *testing.T) {
	cfg := azureKeyConfig("https://my-res.openai.azure.com")
	full := []AzureRegisteredModel{
		entry("aaaaaaaaa1", AzureModelKindChat, "zeta"),
		entry("aaaaaaaaa2", AzureModelKindClaude, "Sonnet"),
		entry("aaaaaaaaa3", AzureModelKindChat, "Alpha"),
		entry("aaaaaaaaa4", AzureModelKindAgent, "Helper"),
		entry("aaaaaaaaa5", AzureModelKindEmbedding, "emb"),
		entry("aaaaaaaaa6", AzureModelKindChat, "beta"),
	}
	reversed := make([]AzureRegisteredModel, len(full))
	for i, m := range full {
		reversed[len(full)-1-i] = m
	}
	picker := registryTestProvider(t, nil, cfg, full)
	chat := registryTestProvider(t, nil, cfg, reversed)
	if got := strings.Join(picker.cfg.Deployments, ","); got != "Alpha,beta,zeta" {
		t.Errorf("chat deployments = %s, want sorted case-insensitively", got)
	}
	if got := strings.Join(picker.cfg.ClaudeDeployments, ","); got != "Sonnet" {
		t.Errorf("claude deployments = %s", got)
	}
	if azureModelsCacheKey(picker.cfg) != azureModelsCacheKey(chat.cfg) {
		t.Error("the same entry set in another order must key the same listing")
	}

	// Only what shapes the listing moves the key: a changed deployment and
	// another credential do; an agent entry does not (agents come from the
	// project listing, not the document).
	withAgent := registryTestProvider(t, nil, cfg, append(append([]AzureRegisteredModel{}, full...), entry("aaaaaaaaa7", AzureModelKindAgent, "Other")))
	if azureModelsCacheKey(withAgent.cfg) != azureModelsCacheKey(picker.cfg) {
		t.Error("an agent entry must not change the listing key")
	}
	renamed := append([]AzureRegisteredModel{}, full...)
	renamed[0].Deployment = "eta"
	if azureModelsCacheKey(registryTestProvider(t, nil, cfg, renamed).cfg) == azureModelsCacheKey(picker.cfg) {
		t.Error("a changed deployment must change the listing key")
	}
	otherKey := cfg
	otherKey.Auth.Key = "k2"
	if azureModelsCacheKey(registryTestProvider(t, nil, otherKey, full).cfg) == azureModelsCacheKey(picker.cfg) {
		t.Error("another credential must never share a listing")
	}

	messy := []AzureRegisteredModel{
		entry("bbbbbbbbb1", AzureModelKindChat, "  gpt-4o "),                                      // trimmed
		entry("bbbbbbbbb2", AzureModelKindClaude, "GPT-4O"),                                       // same name space as the chat entry
		entry("bbbbbbbbb3", "vision", "x"),                                                        // unknown kind
		entry("bbbbbbbbb4", AzureModelKindChat, "bad name"),                                       // invalid name
		{EntryID: "bbbbbbbbb5", Kind: AzureModelKindChat, Deployment: "gpt-4o", ContextWindow: 5}, // duplicate
		{EntryID: "bbbbbbbbb6", Kind: AzureModelKindChat, Deployment: "small", ContextWindow: 12, MaxOutputTokens: 5_000_000},
		{EntryID: "bbbbbbbbb7", Kind: AzureModelKindEmbedding, Deployment: "emb", ContextWindow: 8192, MaxOutputTokens: 512},
		entry("bbbbbbbbb8", AzureModelKindEmbedding, "gpt-4o"), // its own name space
	}
	p := registryTestProvider(t, nil, cfg, messy)
	var got []string
	for _, m := range p.registry {
		got = append(got, fmt.Sprintf("%s:%s:%d:%d", m.Kind, m.Deployment, m.ContextWindow, m.MaxOutputTokens))
	}
	if want := "chat:gpt-4o:0:0,chat:small:0:0,embedding:emb:0:0,embedding:gpt-4o:0:0"; strings.Join(got, ",") != want {
		t.Errorf("registry = %s, want %s", strings.Join(got, ","), want)
	}
	if len(p.cfg.ClaudeDeployments) != 0 || strings.Join(p.cfg.Deployments, ",") != "gpt-4o,small" {
		t.Errorf("deployments = %v / claude %v", p.cfg.Deployments, p.cfg.ClaudeDeployments)
	}
}

// Claude entries answer on the Anthropic route as "claude:<name>", chat
// entries on /openai/v1; a chat entry the cached project listing re-lists as
// a Claude deployment is routed to the Anthropic route too.
func TestAzureConnectionProvider_ClaudeRouting(t *testing.T) {
	t.Run("claude entry", func(t *testing.T) {
		srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
			switch r.URL.Path {
			case "/anthropic/v1/messages":
				writeJSON(w, 200, map[string]any{"type": "message", "model": "claude-sonnet-4-6", "stop_reason": "end_turn",
					"content": []map[string]any{{"type": "text", "text": "hi"}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}})
			case "/openai/v1/chat/completions":
				writeJSON(w, 200, chatCompletionJSON("hi", "gpt-4o-2024-11-20"))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		})
		ResetAzureFoundryCaches()
		p := registryTestProvider(t, srv, azureKeyConfig(srv.URL), []AzureRegisteredModel{
			entry("aaaaaaaaa1", AzureModelKindChat, "gpt"), entry("aaaaaaaaa2", AzureModelKindClaude, "sonnet"),
		})
		for _, model := range []string{"claude:sonnet", "gpt"} {
			if _, err := p.Chat(context.Background(), Request{Model: model, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
				t.Fatalf("%s: %v", model, err)
			}
		}
		reqs := srv.requests()
		if len(reqs) != 2 || reqs[0].Path != "/anthropic/v1/messages" || reqs[0].Body["model"] != "sonnet" ||
			reqs[1].Path != "/openai/v1/chat/completions" || reqs[1].Body["model"] != "gpt" {
			t.Errorf("requests = %+v", reqs)
		}
	})

	t.Run("chat entry re-listed as Claude", func(t *testing.T) {
		srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
			switch r.URL.Path {
			case "/api/projects/p1/deployments":
				writeJSON(w, 200, map[string]any{"value": []map[string]any{
					{"name": "c1", "modelName": "claude-sonnet-4-6", "modelPublisher": "Anthropic"},
				}})
			case "/api/projects/p1/agents":
				writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
			case "/anthropic/v1/messages":
				writeJSON(w, 200, map[string]any{"type": "message", "model": "claude-sonnet-4-6", "stop_reason": "end_turn",
					"content": []map[string]any{{"type": "text", "text": "hi"}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}})
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(404)
			}
		})
		ResetAzureFoundryCaches()
		p := registryTestProvider(t, srv, registryEntraConfig(srv), []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "c1")})
		if err := p.refreshListing(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Chat(context.Background(), Request{Model: "c1", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		last := srv.requests()[len(srv.requests())-1]
		if last.Path != "/anthropic/v1/messages" || last.Body["model"] != "c1" {
			t.Errorf("the re-listed chat entry went to %s (model %v), want the Anthropic route", last.Path, last.Body["model"])
		}
	})
}

// A registry provider's listing never waits on the network: cold, it is the
// entries at once; RefreshAzureFoundryListing fills the cache the chat path
// then reads (one fetch shared by concurrent refreshes, none while live); an
// expired listing is still served for azureModelsStaleGrace.
func TestAzureConnectionProvider_ListingIsCacheOnly(t *testing.T) {
	var projectCalls, deploymentsCalls atomic.Int32
	release := make(chan struct{})
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		projectCalls.Add(1)
		if r.URL.Path == "/api/projects/p1/deployments" {
			deploymentsCalls.Add(1)
		}
		<-release
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{{"name": "prod-chat", "modelName": "gpt-4.1"}}})
		case "/api/projects/p1/agents":
			helperAgents(w)
		}
	})
	ResetAzureFoundryCaches()
	entries := []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "prod-chat"), entry("aaaaaaaaa2", AzureModelKindAgent, "Helper")}
	p := registryTestProvider(t, srv, registryEntraConfig(srv), entries)

	start := time.Now()
	cold, err := p.Models(context.Background())
	if err != nil || modelIDs(cold) != "prod-chat" || time.Since(start) > time.Second {
		t.Fatalf("cold listing = %s, %v after %v; want the entries at once", modelIDs(cold), err, time.Since(start))
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("the cold listing made %d requests, want none", n)
	}

	// Two refreshes (the picker path and the chat path's entry set, in
	// another order) share one fetch.
	other := registryTestProvider(t, srv, registryEntraConfig(srv), []AzureRegisteredModel{entries[1], entries[0]})
	done := make(chan error, 2)
	go func() { done <- p.refreshListing(context.Background()) }()
	go func() { done <- other.refreshListing(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := deploymentsCalls.Load(); n != 1 {
		t.Errorf("deployments GETs = %d, want one fetch for both refreshes", n)
	}
	calls := projectCalls.Load()

	warm, _ := p.Models(context.Background())
	if !hasModel(warm, "agent:Helper") {
		t.Fatalf("warm listing = %s, want the project's agents", modelIDs(warm))
	}
	for _, m := range warm {
		if m.ID == "prod-chat" && (m.ServedModel != "gpt-4.1" || m.ContextLimit != 1047576) {
			t.Errorf("prod-chat = %+v, want the project listing's model and window", m)
		}
	}
	if err := p.refreshListing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := projectCalls.Load(); n != calls {
		t.Errorf("a refresh of a live listing made %d requests", n-calls)
	}

	// Expired: still served within the grace — and a refresh asks again.
	expireAzureListing(t, p.cfg)
	if stale, _ := p.Models(context.Background()); !hasModel(stale, "agent:Helper") {
		t.Errorf("an expired listing within the grace = %s, want it served", modelIDs(stale))
	}
	if _, ok := p.cachedListing(); !ok {
		t.Error("routing must see the stale listing too")
	}
	azureModelsCache.mu.Lock()
	e := azureModelsCache.m[azureModelsCacheKey(p.cfg)]
	e.expires = time.Now().Add(-azureModelsStaleGrace - time.Second)
	azureModelsCache.m[azureModelsCacheKey(p.cfg)] = e
	azureModelsCache.mu.Unlock()
	if gone, _ := p.Models(context.Background()); modelIDs(gone) != "prod-chat" {
		t.Errorf("a listing past the grace = %s, want the entries only", modelIDs(gone))
	}
	if err := p.refreshListing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if projectCalls.Load() == calls {
		t.Error("an expired listing must be refreshed")
	}
}

// A resource that cannot list its project refreshes nothing; an invalid
// document says so.
func TestRefreshAzureFoundryListing_NothingToRefresh(t *testing.T) {
	const keyDoc = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"}}`
	if err := RefreshAzureFoundryListing(context.Background(), keyDoc, []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "gpt-4o")}); err != nil {
		t.Errorf("API key: %v, want nil (nothing to list, no request)", err)
	}
	err := RefreshAzureFoundryListing(context.Background(), `{"endpoint":"https://evil.example.com","auth":{"mode":"api_key","key":"k"}}`, nil)
	if !errors.Is(err, ErrProviderConfigInvalid) || azureCfgCode(t, err) != AzureCfgErrHostNotAllowed {
		t.Errorf("invalid document: %v, want ErrProviderConfigInvalid / HOST_NOT_ALLOWED", err)
	}
}

// Forgetting a credential drops every listing built on it — a registry
// resource's is keyed by its entries, which the caller does not know — and no
// other credential's.
func TestForgetAzureFoundryCredential_DropsRegistryListings(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	cfg := AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/p",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: entraTestTenant, ClientID: azureSeamClientID, ClientSecret: "s1"}}
	other := cfg
	other.Auth.ClientSecret = "s2"
	a, b := cfg, cfg
	a.Deployments = []string{"one"}
	b.Deployments, b.ClaudeDeployments = []string{"two"}, []string{"three"}
	o := other
	o.Deployments = []string{"one"}
	for _, c := range []AzureFoundryConfig{a, b, o} {
		storeAzureModels(azureModelsCacheKey(c), []ModelInfo{{ID: "x"}}, time.Hour)
	}
	ForgetAzureFoundryCredential(cfg)
	for name, c := range map[string]AzureFoundryConfig{"a": a, "b": b} {
		if _, ok := cachedAzureModels(azureModelsCacheKey(c)); ok {
			t.Errorf("listing %s on the forgotten credential survived", name)
		}
	}
	if _, ok := cachedAzureModels(azureModelsCacheKey(o)); !ok {
		t.Error("another credential's listing must survive")
	}
}

// Registry text allows only what shows: a name of fillers, blanks,
// variation selectors or unassigned code points — or a visible name with
// one of them hidden in it — is refused, and every name needs a visible
// character (review S5).
func TestAzureTextRunes_RefuseWhatDoesNotShow(t *testing.T) {
	for _, r := range []rune{0x3164, 0x115F, 0x1160, 0xFFA0, 0x034F, 0x2800, 0xFE0F, 0xE0100, 0x180B,
		0xFFF0, 0x0378, 0xE000, 0x10FFFD, 0x200B, 0x202E, 0x00AD, 0x0000, 0x001B} {
		if AzureTextRuneAllowed(r) || AzureTextRuneVisible(r) {
			t.Errorf("U+%04X allowed", r)
		}
		if ValidAzureAgentName("bot" + string(r)) {
			t.Errorf("agent name with U+%04X accepted", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '7', '-', '(', '€', '☕', 0x65E5, 0x00E9, 0x2211} {
		if !AzureTextRuneAllowed(r) || !AzureTextRuneVisible(r) {
			t.Errorf("U+%04X refused", r)
		}
	}
	// Spaces and combining marks are allowed, not visible on their own.
	for _, r := range []rune{' ', 0x0301} {
		if !AzureTextRuneAllowed(r) || AzureTextRuneVisible(r) {
			t.Errorf("U+%04X: allowed %v visible %v, want allowed but not visible", r, AzureTextRuneAllowed(r), AzureTextRuneVisible(r))
		}
	}
	if ValidAzureAgentName(string(rune(0x0301))) || !ValidAzureAgentName("Review bot") || !ValidAzureAgentName("e"+string(rune(0x0301))) {
		t.Error("agent names need one visible character")
	}
	if err := ValidateAzureRegisteredModel(AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: string(rune(0x3164))}); err == nil {
		t.Error("an agent named with a Hangul filler validated")
	}
}
