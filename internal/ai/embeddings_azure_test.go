package ai

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"pad-analyzer/internal/config"
)

// azureEmbedServer answers /embeddings with one 2-wide vector per input,
// except that a request carrying `dimensions` gets reject's status/body when
// reject is non-nil (the model refusing the parameter).
func azureEmbedServer(t *testing.T, reject func(w http.ResponseWriter)) *azureTestServer {
	t.Helper()
	// A refusal is memoised per endpoint + model (embedDimsRefusals); a
	// reused httptest port must not leak one into the next test.
	resetEmbedDimsRefusals()
	t.Cleanup(resetEmbedDimsRefusals)
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		if r.URL.Path != "/openai/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if _, sent := body["dimensions"]; sent && reject != nil {
			reject(w)
			return
		}
		inputs, _ := body["input"].([]any)
		out := []map[string]any{}
		for range inputs {
			out = append(out, map[string]any{"embedding": []float32{0.1, 0.2}})
		}
		writeJSON(w, 200, map[string]any{"data": out})
	})
}

func rejectDimensions(w http.ResponseWriter) {
	writeJSON(w, 400, map[string]any{"error": map[string]any{
		"message": "This model does not support specifying dimensions.",
		"type":    "invalid_request_error",
	}})
}

// TestEmbeddingDimensionsFor pins #12: `dimensions` only for the
// text-embedding-3 family and never above the model's native width (asking
// -small for 3072 is a 400; Matryoshka truncation only shortens).
func TestEmbeddingDimensionsFor(t *testing.T) {
	cases := []struct {
		model string
		n     int
		want  int
	}{
		{"text-embedding-3-small", 3072, 0},
		{"text-embedding-3-small", 1536, 1536},
		{"text-embedding-3-small", 512, 512},
		{"text-embedding-3-large", 3072, 3072},
		{"text-embedding-3-large", 1536, 1536},
		{"text-embedding-3-large", 4096, 0},
		{"Text-Embedding-3-Small", 1024, 1024},
		{"text-embedding-3-prod", 3072, 3072}, // family, size unknown
		{"text-embedding-3-small", 0, 0},
		{"text-embedding-ada-002", 1536, 0},
		{"embedding-3", 1536, 0}, // GLM
		{"gemini-embedding-001", 1536, 0},
	}
	for _, tc := range cases {
		if got := embeddingDimensionsFor(tc.model, tc.n); got != tc.want {
			t.Errorf("embeddingDimensionsFor(%q, %d) = %d, want %d", tc.model, tc.n, got, tc.want)
		}
	}
}

// TestEmbed_NeverAboveNativeWidth: PAD_EMBEDDING_DIM=3072 (set for a
// -large or Gemini deployment) must not break a user whose embedder is
// OpenAI's default text-embedding-3-small — it gets its native vectors.
func TestEmbed_NeverAboveNativeWidth(t *testing.T) {
	srv, seen := dimsEmbedServer(t)
	defer srv.Close()
	base := srv.URL + "/chat/completions"

	small := &OpenAIProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "openai", embeddingModel: "text-embedding-3-small"}}
	small.setEmbeddingDimensions(3072)
	runEmbed(t, small, []string{"a"})

	large := &OpenAIProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "openai", embeddingModel: "text-embedding-3-large"}}
	large.setEmbeddingDimensions(3072)
	runEmbed(t, large, []string{"a"})

	if len(*seen) != 2 {
		t.Fatalf("got %d requests, want 2", len(*seen))
	}
	if (*seen)[0] != nil {
		t.Errorf("text-embedding-3-small with 3072 must send no dimensions, got %v", (*seen)[0])
	}
	if (*seen)[1] != float64(3072) {
		t.Errorf("text-embedding-3-large with 3072: dimensions = %v, want 3072", (*seen)[1])
	}
}

// TestAzureFoundry_EmbedCustomDeploymentNameSendsDimensionsThenRetries pins
// #11/#12 for Azure: a deployment name that reveals nothing ("kb-embed") still
// gets the configured width, and a model that refuses the parameter costs
// exactly one extra request, the second without it.
func TestAzureFoundry_EmbedCustomDeploymentNameSendsDimensionsThenRetries(t *testing.T) {
	srv := azureEmbedServer(t, rejectDimensions)
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	p.setEmbeddingDimensions(1536)

	vecs, err := p.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want exactly 2 (one repair round trip)", len(reqs))
	}
	if reqs[0].Body["dimensions"] != float64(1536) || reqs[0].Body["model"] != "kb-embed" {
		t.Errorf("first request = %v, want dimensions 1536 for the custom deployment", reqs[0].Body)
	}
	if _, sent := reqs[1].Body["dimensions"]; sent {
		t.Errorf("retry still carries dimensions: %v", reqs[1].Body)
	}

	// A model that accepts it: one request, the width honoured.
	ok := azureEmbedServer(t, nil)
	q := newAzureKeyProvider(t, ok, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	q.setEmbeddingDimensions(1536)
	if _, err := q.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if r := ok.requests(); len(r) != 1 || r[0].Body["dimensions"] != float64(1536) {
		t.Errorf("accepting model: requests = %+v, want one with dimensions 1536", r)
	}
}

// TestAzureFoundry_EmbedOtherErrorsAreNotRetried: only a refusal naming
// `dimensions` is repaired; any other 400 surfaces after one request, and a
// second refusal after the repair is returned rather than looped on.
func TestAzureFoundry_EmbedOtherErrorsAreNotRetried(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid input type"}})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	p.setEmbeddingDimensions(1536)
	if _, err := p.Embed(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "invalid input type") {
		t.Fatalf("want the 400 surfaced, got %v", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests, want 1 (not a dimensions refusal)", n)
	}

	always := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		rejectDimensions(w)
	})
	q := newAzureKeyProvider(t, always, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	q.setEmbeddingDimensions(1536)
	if _, err := q.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("want an error when the repaired request is refused too")
	}
	if n := len(always.requests()); n != 2 {
		t.Errorf("%d requests, want 2 (one repair, never a loop)", n)
	}
}

// TestAzureEmbeddingDimensions: deployment names that do reveal the model
// follow the OpenAI rules; ada (fixed width) never gets the parameter.
func TestAzureEmbeddingDimensions(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want int
	}{
		{"kb-embed", 1536, 1536},
		{"embeddings", 3072, 3072},
		{"text-embedding-3-small", 3072, 0},
		{"text-embedding-3-large", 3072, 3072},
		{"text-embedding-ada-002", 1536, 0},
		{"ada002-prod", 1536, 0},
		{"canada-embed", 1536, 1536}, // "ada" inside a word is not ada
		{"kb-embed", 0, 0},
	}
	for _, tc := range cases {
		if got := azureEmbeddingDimensions(tc.name, tc.n); got != tc.want {
			t.Errorf("azureEmbeddingDimensions(%q, %d) = %d, want %d", tc.name, tc.n, got, tc.want)
		}
	}
}

// TestForEmbedding_AzureWithoutEmbeddingDeployment pins #16: a keyed Azure
// document that names no embedding deployment is not an embedder — the
// factory says so up front (ErrEmbeddingDeploymentMissing) so the knowledge
// base's resolution moves on; an invalid document reports its config error.
func TestForEmbedding_AzureWithoutEmbeddingDeployment(t *testing.T) {
	ResetAzureFoundryCaches()
	docs := map[string]string{
		"chat-only":  `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o"}`,
		"with-embed": `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o","embeddingDeployment":"kb-embed"}`,
		"invalid":    `{"endpoint":"http://169.254.169.254","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o"}`,
	}
	factoryFor := func(doc string) *ProviderFactory {
		return NewProviderFactory(func(_, provider string) (string, error) {
			if provider == "azure-foundry" {
				return doc, nil
			}
			return "", nil
		}, nil, nil, nil)
	}

	_, err := factoryFor(docs["chat-only"]).ForEmbedding("", "azure-foundry", "")
	if !errors.Is(err, ErrEmbeddingDeploymentMissing) {
		t.Errorf("chat-only document: err = %v, want ErrEmbeddingDeploymentMissing", err)
	}
	// The deployment-wide AI.EmbeddingModel override counts as a deployment.
	if p, err := factoryFor(docs["chat-only"]).ForEmbedding("", "azure-foundry", "shared-embed"); err != nil || p == nil {
		t.Errorf("override applied: want a provider, got %v", err)
	}
	if p, err := factoryFor(docs["with-embed"]).ForEmbedding("", "azure-foundry", ""); err != nil || p == nil {
		t.Errorf("document with an embedding deployment: want a provider, got %v", err)
	}
	_, err = factoryFor(docs["invalid"]).ForEmbedding("", "azure-foundry", "")
	if !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("invalid document: err = %v, want ErrProviderConfigInvalid", err)
	}
	// Chat is unaffected: For still builds the provider.
	if _, err := factoryFor(docs["chat-only"]).For("", "azure-foundry"); err != nil {
		t.Errorf("For(azure-foundry) on a chat-only document: %v", err)
	}
}

// TestEmbeddingProviderIDs_MatchRegistry keeps config's PAD_EMBEDDING_PROVIDER
// allow-list (config cannot import ai) in sync with the embedding registry.
func TestEmbeddingProviderIDs_MatchRegistry(t *testing.T) {
	got := append([]string(nil), config.EmbeddingProviderIDs...)
	want := EmbeddingFallbackOrder()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("config.EmbeddingProviderIDs = %v, want the embedding-capable providers %v", got, want)
	}
	for _, id := range config.EmbeddingProviderIDs {
		if !SupportsEmbeddings(id) {
			t.Errorf("config accepts %q but it cannot embed", id)
		}
	}
}
