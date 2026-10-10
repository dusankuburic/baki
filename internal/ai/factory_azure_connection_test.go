package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// Part 9: ProviderFactory.ForAzureConnection builds one registry resource's
// provider from exactly the credential its ref names — the caller's own key
// or the org's shared one, never a fallback between the two — with the memo
// scope and usage attribution of that credential.

const connTestDoc = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"}}`

// keyLookups records every key-store read a factory makes.
type keyLookups struct {
	personal, org []string
}

// connTestFactory resolves personal keys from members (scope → key name →
// doc) and org keys from orgs (org → key name → doc), recording each read.
func connTestFactory(members, orgs map[string]map[string]string, recorder UsageRecorder) (*ProviderFactory, *keyLookups) {
	seen := &keyLookups{}
	f := NewProviderFactory(func(scope, key string) (string, error) {
		seen.personal = append(seen.personal, scope+"|"+key)
		return members[scope][key], nil
	}, nil, recorder, nil)
	f.SetOrgKeyLookup(func(orgID, key string) (string, error) {
		seen.org = append(seen.org, orgID+"|"+key)
		return orgs[orgID][key], nil
	})
	return f, seen
}

// connChain unwraps a factory chain into its audited wrapper and raw
// registry provider.
func connChain(t *testing.T, p Provider) (*auditedProvider, *AzureFoundryProvider) {
	t.Helper()
	ap, ok := p.(*auditedProvider)
	if !ok {
		t.Fatalf("provider is %T, want the audited chain", p)
	}
	raw, ok := unwrapProvider(ap).(*AzureFoundryProvider)
	if !ok || !raw.fromRegistry {
		t.Fatalf("raw provider is %T (registry %v), want a registry *AzureFoundryProvider", unwrapProvider(ap), ok && raw.fromRegistry)
	}
	return ap, raw
}

func TestForAzureConnection_PersonalRef(t *testing.T) {
	f, seen := connTestFactory(map[string]map[string]string{"u1": {
		"azure-foundry/abc123def4": connTestDoc,
		"azure-foundry":            strings.Replace(connTestDoc, "my-res", "legacy-res", 1),
	}}, map[string]map[string]string{"org-1": {"azure-foundry/abc123def4": connTestDoc}}, nil)
	models := []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "gpt-4o"), entry("aaaaaaaaa2", AzureModelKindClaude, "sonnet")}

	p, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{ConnID: "abc123def4"}, models)
	if err != nil {
		t.Fatalf("ForAzureConnection: %v", err)
	}
	ap, raw := connChain(t, p)
	if strings.Join(seen.personal, ",") != "u1|azure-foundry/abc123def4" || len(seen.org) != 0 {
		t.Errorf("lookups = personal %v org %v, want the caller's resource key only", seen.personal, seen.org)
	}
	if ap.scope != "u1" || ap.memoScope != "u1\x00https://my-res.openai.azure.com" || ap.usageOrg != "" || UsedOrgCredential(p) {
		t.Errorf("chain scope %q memo %q usageOrg %q org=%v, want the caller's own credential", ap.scope, ap.memoScope, ap.usageOrg, UsedOrgCredential(p))
	}
	if got := strings.Join(raw.cfg.Deployments, ",") + "|" + strings.Join(raw.cfg.ClaudeDeployments, ","); got != "gpt-4o|sonnet" || raw.configErr != nil {
		t.Errorf("raw provider deployments = %s (err %v), want the entries", got, raw.configErr)
	}

	// The default resource is the legacy key.
	p, err = f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{ConnID: AzureDefaultConnectionID}, models)
	if err != nil {
		t.Fatal(err)
	}
	if _, raw := connChain(t, p); raw.cfg.Endpoint != "https://legacy-res.openai.azure.com" || seen.personal[len(seen.personal)-1] != "u1|azure-foundry" {
		t.Errorf("default resource read %v → %q", seen.personal, raw.cfg.Endpoint)
	}
}

func TestForAzureConnection_OrgRef(t *testing.T) {
	f, seen := connTestFactory(map[string]map[string]string{"u1": {"azure-foundry/abc123def4": connTestDoc}},
		map[string]map[string]string{"org-1": {"azure-foundry/abc123def4": strings.Replace(connTestDoc, "my-res", "shared-res", 1)}}, nil)
	p, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, nil)
	if err != nil {
		t.Fatalf("ForAzureConnection: %v", err)
	}
	ap, raw := connChain(t, p)
	if strings.Join(seen.org, ",") != "org-1|azure-foundry/abc123def4" || len(seen.personal) != 0 {
		t.Errorf("lookups = personal %v org %v, want the org's resource key only", seen.personal, seen.org)
	}
	if raw.cfg.Endpoint != "https://shared-res.openai.azure.com" {
		t.Errorf("endpoint = %q, want the org's document", raw.cfg.Endpoint)
	}
	if ap.scope != "u1" || ap.memoScope != OrgScope("org-1")+"\x00https://shared-res.openai.azure.com" || ap.usageOrg != "org-1" || !UsedOrgCredential(p) {
		t.Errorf("chain scope %q memo %q usageOrg %q org=%v, want usage by the member, memos and budget on the org", ap.scope, ap.memoScope, ap.usageOrg, UsedOrgCredential(p))
	}
}

// No member-first fallback: the registry resolved WHICH credential the
// request runs on, so a missing one is ErrKeyNotConfigured — the other side
// is never consulted, either way.
func TestForAzureConnection_NoCredentialFallback(t *testing.T) {
	ref := AzureConnectionRef{ConnID: "abc123def4"}
	f, seen := connTestFactory(nil, map[string]map[string]string{"org-1": {"azure-foundry/abc123def4": connTestDoc}}, nil)
	if _, err := f.ForAzureConnection(context.Background(), "u1", ref, nil); !errors.Is(err, ErrKeyNotConfigured) || len(seen.org) != 0 {
		t.Errorf("personal ref without a personal key: err %v, org lookups %v; want ErrKeyNotConfigured and no org read", err, seen.org)
	}

	f, seen = connTestFactory(map[string]map[string]string{"u1": {"azure-foundry/abc123def4": connTestDoc}}, nil, nil)
	if _, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, nil); !errors.Is(err, ErrKeyNotConfigured) || len(seen.personal) != 0 {
		t.Errorf("org ref without an org key: err %v, personal lookups %v; want ErrKeyNotConfigured and no personal read", err, seen.personal)
	}

	noStore := NewProviderFactory(func(_, _ string) (string, error) { return connTestDoc, nil }, nil, nil, nil)
	if _, err := noStore.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, nil); !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("org ref without an org store: %v, want ErrKeyNotConfigured", err)
	}

	// A key-store failure is reported, never answered with another key.
	memberErr := errors.New("keystore: message authentication failed")
	f = NewProviderFactory(func(_, _ string) (string, error) { return "", memberErr }, nil, nil, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { return connTestDoc, nil })
	if _, err := f.ForAzureConnection(context.Background(), "u1", ref, nil); !errors.Is(err, memberErr) {
		t.Errorf("member lookup error: %v, want it surfaced", err)
	}
	orgErr := errors.New("db down")
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { return "", orgErr })
	if _, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, nil); !errors.Is(err, orgErr) || !strings.Contains(err.Error(), "org key") {
		t.Errorf("org lookup error: %v, want it surfaced as the org's", err)
	}

	// An id no resource can have never becomes a key-store read.
	f, seen = connTestFactory(nil, nil, nil)
	for _, id := range []string{"", "Default", "../../x", "abc123def4/x", "ABC123DEF4"} {
		if _, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{ConnID: id}, nil); err == nil || errors.Is(err, ErrKeyNotConfigured) {
			t.Errorf("resource id %q: err %v, want a refusal", id, err)
		}
	}
	if len(seen.personal)+len(seen.org) != 0 {
		t.Errorf("invalid ids read keys: %v %v", seen.personal, seen.org)
	}
}

// The knowledge-base embedder on one resource: the designated embedding
// entry's deployment at the factory's vector width; anything but an
// embedding entry is "no embedding deployment" (the org's variant on an org
// ref).
func TestForAzureConnectionEmbedding(t *testing.T) {
	f, _ := connTestFactory(map[string]map[string]string{"u1": {"azure-foundry/abc123def4": connTestDoc}},
		map[string]map[string]string{"org-1": {"azure-foundry/abc123def4": connTestDoc}}, nil)
	f.SetEmbeddingDimensions(512)
	emb := AzureRegisteredModel{EntryID: "aaaaaaaaa1", Kind: AzureModelKindEmbedding, Deployment: "kb-embed"}

	p, err := f.ForAzureConnectionEmbedding(context.Background(), "u1", AzureConnectionRef{ConnID: "abc123def4"}, emb)
	if err != nil {
		t.Fatalf("ForAzureConnectionEmbedding: %v", err)
	}
	ap, raw := connChain(t, p)
	if raw.EmbeddingModel() != "kb-embed" || raw.cfg.EmbeddingDeployment != "kb-embed" || raw.embedDimensions != 512 {
		t.Errorf("embedder model %q deployment %q dims %d", raw.EmbeddingModel(), raw.cfg.EmbeddingDeployment, raw.embedDimensions)
	}
	if ap.usageOrg != "" || ap.embedNamer == nil || ap.embedNamer.EmbeddingModel() != "kb-embed" {
		t.Errorf("chain usageOrg %q namer %v", ap.usageOrg, ap.embedNamer)
	}

	p, err = f.ForAzureConnectionEmbedding(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, emb)
	if err != nil {
		t.Fatal(err)
	}
	if ap, _ := connChain(t, p); ap.usageOrg != "org-1" || ap.memoScope != OrgScope("org-1")+"\x00https://my-res.openai.azure.com" {
		t.Errorf("org embedder usageOrg %q memo %q", ap.usageOrg, ap.memoScope)
	}

	chat := AzureRegisteredModel{EntryID: "aaaaaaaaa2", Kind: AzureModelKindChat, Deployment: "gpt-4o"}
	if _, err := f.ForAzureConnectionEmbedding(context.Background(), "u1", AzureConnectionRef{ConnID: "abc123def4"}, chat); !errors.Is(err, ErrEmbeddingDeploymentMissing) || errors.Is(err, ErrOrgEmbeddingDeploymentMissing) {
		t.Errorf("a chat entry: %v, want ErrEmbeddingDeploymentMissing", err)
	}
	if _, err := f.ForAzureConnectionEmbedding(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, AzureRegisteredModel{Kind: AzureModelKindEmbedding}); !errors.Is(err, ErrOrgEmbeddingDeploymentMissing) {
		t.Errorf("an org ref without a deployment: %v, want ErrOrgEmbeddingDeploymentMissing", err)
	}
}

// End to end on the in-process Azure: a chat on an org resource records its
// usage row against the org (the request carried no org), a chat on the
// member's own resource against the member alone, and an embedding on the org
// resource against the org — each on the entry's deployment.
func TestForAzureConnection_UsageFollowsTheCredential(t *testing.T) {
	seam := installAzureSeam(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/chat/completions":
			writeJSON(w, 200, chatCompletionJSON("hi", "gpt-4o-2024-11-20"))
		case "/openai/v1/embeddings":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ch := make(chan *interfaces.UsageMetric, 8)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	f, _ := connTestFactory(map[string]map[string]string{"u1": {"azure-foundry/abc123def4": seamKeyDoc("own")}},
		map[string]map[string]string{"org-1": {"azure-foundry/abc123def4": seamKeyDoc("shared")}}, rec)
	models := []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "gpt-4o")}

	org, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"}, models)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.Chat(context.Background(), Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("org chat: %v", err)
	}
	if m := nextUsage(t, ch); m.OrgID != "org-1" || m.UserID != "u1" || m.Model != "gpt-4o" {
		t.Errorf("org usage row = org %q user %q model %q, want org-1 / u1 / gpt-4o", m.OrgID, m.UserID, m.Model)
	}

	own, err := f.ForAzureConnection(context.Background(), "u1", AzureConnectionRef{ConnID: "abc123def4"}, models)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.Chat(context.Background(), Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("own chat: %v", err)
	}
	if m := nextUsage(t, ch); m.OrgID != "" || m.UserID != "u1" {
		t.Errorf("own usage row = org %q user %q, want personal", m.OrgID, m.UserID)
	}

	embedder, err := f.ForAzureConnectionEmbedding(context.Background(), "u1", AzureConnectionRef{OrgID: "org-1", ConnID: "abc123def4"},
		AzureRegisteredModel{EntryID: "aaaaaaaaa2", Kind: AzureModelKindEmbedding, Deployment: "kb-embed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"chunk"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if m := nextUsage(t, ch); m.OrgID != "org-1" || m.Model != "kb-embed" {
		t.Errorf("embedding usage row = org %q model %q, want org-1 / kb-embed", m.OrgID, m.Model)
	}

	hosts := map[string]string{}
	for _, r := range seam.requests("") {
		hosts[r.Path+"@"+r.Host] = r.JSON()["model"].(string)
	}
	if hosts["/openai/v1/chat/completions@shared.unit-test.invalid"] != "gpt-4o" || hosts["/openai/v1/chat/completions@own.unit-test.invalid"] != "gpt-4o" ||
		hosts["/openai/v1/embeddings@shared.unit-test.invalid"] != "kb-embed" {
		t.Errorf("requests = %v, want each call on its own resource with the entry's deployment", hosts)
	}
}
