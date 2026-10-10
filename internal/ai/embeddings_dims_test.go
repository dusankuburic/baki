package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// dimsEmbedServer records the `dimensions` field of every /embeddings request.
func dimsEmbedServer(t *testing.T) (*httptest.Server, *[]any) {
	t.Helper()
	var seen []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body["dimensions"])
		inputs, _ := body["input"].([]any)
		out := []map[string]any{}
		for range inputs {
			out = append(out, map[string]any{"embedding": []float32{0.1, 0.2}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	return srv, &seen
}

func TestEmbed_DimensionsOnlyForTextEmbedding3(t *testing.T) {
	srv, seen := dimsEmbedServer(t)
	defer srv.Close()
	base := srv.URL + "/chat/completions"

	openai := &OpenAIProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "openai", embeddingModel: "text-embedding-3-large"}}
	openai.setEmbeddingDimensions(1536)
	runEmbed(t, openai, []string{"a"})

	glm := &GLMProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "glm", embeddingModel: "embedding-3"}}
	glm.setEmbeddingDimensions(1536)
	runEmbed(t, glm, []string{"a"})

	unset := &OpenAIProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "openai", embeddingModel: "text-embedding-3-small"}}
	runEmbed(t, unset, []string{"a"})

	if len(*seen) != 3 {
		t.Fatalf("got %d requests", len(*seen))
	}
	if (*seen)[0] != float64(1536) {
		t.Errorf("text-embedding-3-large request dimensions = %v, want 1536", (*seen)[0])
	}
	if (*seen)[1] != nil {
		t.Errorf("embedding-3 (GLM) must not receive dimensions, got %v", (*seen)[1])
	}
	if (*seen)[2] != nil {
		t.Errorf("provider without a configured width must not send dimensions, got %v", (*seen)[2])
	}
}

func TestForEmbedding_AppliesDimensions(t *testing.T) {
	srv, seen := dimsEmbedServer(t)
	defer srv.Close()
	prev := openAIBaseURL
	openAIBaseURL = srv.URL + "/chat/completions"
	t.Cleanup(func() { openAIBaseURL = prev })

	f := NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	f.SetEmbeddingDimensions(1024)
	p, err := f.ForEmbedding(context.Background(), "", "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0] != float64(1024) {
		t.Errorf("ForEmbedding did not pass the configured width: %v", *seen)
	}
}
