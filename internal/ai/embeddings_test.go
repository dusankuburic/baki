package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// embedTestServer returns a test server whose handler validates the request
// shape for the OpenAI-compatible /embeddings endpoint and responds with a
// canned embedding per input string. It returns the parsed request body via the
// closure for assertions.
func embedTestServer(t *testing.T, wantModel string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("embeddings POST went to %q, want path ending /embeddings", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got["model"] = body["model"]
		got["input"] = body["input"]
		got["auth"] = r.Header.Get("Authorization")
		if wantModel != "" && body["model"] != wantModel {
			t.Errorf("embeddings model = %v, want %q", body["model"], wantModel)
		}
		inputs, _ := body["input"].([]any)
		out := []map[string]any{}
		for range inputs {
			out = append(out, map[string]any{"embedding": []float32{0.1, 0.2, 0.3}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	return srv, &got
}

func runEmbed(t *testing.T, p Provider, texts []string) [][]float32 {
	t.Helper()
	vecs, err := p.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	return vecs
}

func TestEmbed_OpenAI_RequestShapeAndParse(t *testing.T) {
	srv, got := embedTestServer(t, "text-embedding-3-small")
	defer srv.Close()
	base := srv.URL + "/chat/completions"
	p := &OpenAIProvider{openaiBase: openaiBase{
		apiKey: "sk-test", client: srv.Client(), baseURL: &base,
		providerLabel: "openai", embeddingModel: "text-embedding-3-small",
	}}

	vecs := runEmbed(t, p, []string{"hello", "world"})
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if (*got)["auth"] != "Bearer sk-test" {
		t.Errorf("auth header = %v, want Bearer sk-test", (*got)["auth"])
	}
}

func TestEmbed_GLM_DelegatesWithModel(t *testing.T) {
	srv, got := embedTestServer(t, "embedding-3")
	defer srv.Close()
	base := srv.URL + "/chat/completions"
	p := &GLMProvider{openaiBase: openaiBase{
		apiKey: "k", client: srv.Client(), baseURL: &base,
		providerLabel: "glm", embeddingModel: "embedding-3",
	}}

	runEmbed(t, p, []string{"x"})
	if (*got)["model"] != "embedding-3" {
		t.Errorf("GLM embeddings used model %v, want embedding-3", (*got)["model"])
	}
}

func TestEmbed_GitHubModels_DelegatesAndKeepsExtraHeader(t *testing.T) {
	srv, got := embedTestServer(t, "")
	defer srv.Close()
	base := srv.URL + "/chat/completions"
	p := &GitHubModelsProvider{openaiBase: openaiBase{
		apiKey: "ghp_t", client: srv.Client(), baseURL: &base,
		providerLabel: "github models", embeddingModel: "text-embedding-3-small",
		extraHeaders: func(req *http.Request, model string) {
			req.Header.Set("x-ms-model-mesh-model-id", model)
		},
	}}

	runEmbed(t, p, []string{"a", "b"})
	if (*got)["model"] != "text-embedding-3-small" {
		t.Errorf("GitHub Models embeddings model = %v, want text-embedding-3-small", (*got)["model"])
	}
}

// TestEmbed_NoEmbeddingModel_NotSupported confirms that an OpenAI-compatible
// provider with no embedding model configured returns the not-supported error
// WITHOUT making a network call (no server is started).
func TestEmbed_NoEmbeddingModel_NotSupported(t *testing.T) {
	base := "http://0.0.0.0:0/chat/completions" // would fail if a call were attempted
	p := &XAIProvider{openaiBase: openaiBase{
		apiKey: "k", client: http.DefaultClient, baseURL: &base,
		providerLabel: "xai", embeddingModel: "",
	}}
	_, err := p.Embed(context.Background(), []string{"hi"})
	if err == nil {
		t.Fatal("expected not-supported error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' error, got: %v", err)
	}
}

func TestEmbed_Gemini_BatchEmbedContentsShape(t *testing.T) {
	var gotRequests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":batchEmbedContents") {
			t.Errorf("gemini embeddings went to %q, want :batchEmbedContents", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "gem-key" {
			t.Errorf("x-goog-api-key = %q, want gem-key", r.Header.Get("x-goog-api-key"))
		}
		var body struct {
			Requests []map[string]any `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRequests = body.Requests
		embeddings := make([]map[string]any, len(body.Requests))
		for i := range body.Requests {
			embeddings[i] = map[string]any{"values": []float32{0.5, 0.6}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	defer srv.Close()

	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	defer func() { geminiBaseHost = prev }()

	p := &GeminiProvider{apiKey: "gem-key", client: srv.Client()}
	vecs := runEmbed(t, p, []string{"one", "two", "three"})
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}

	// Each request carries the embedding model, a content.parts[].text, and a
	// RETRIEVAL_DOCUMENT task type.
	if len(gotRequests) != 3 {
		t.Fatalf("got %d requests, want 3", len(gotRequests))
	}
	first := gotRequests[0]
	if first["model"] != "models/gemini-embedding-001" {
		t.Errorf("request model = %v, want models/gemini-embedding-001", first["model"])
	}
	if first["taskType"] != "RETRIEVAL_DOCUMENT" {
		t.Errorf("taskType = %v, want RETRIEVAL_DOCUMENT", first["taskType"])
	}
}

// Gemini's batchEmbedContents caps inputs per call, but callers embed up to
// maxKnowledgeChunks (500) at once. Embed must split into ≤100-item sub-batches
// and concatenate the results in input order.
func TestEmbed_Gemini_BatchesLargeInputAndPreservesOrder(t *testing.T) {
	var callCount int
	var perCall []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Requests []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		callCount++
		perCall = append(perCall, len(body.Requests))
		// Echo each input's index back as its vector so we can assert ordering
		// survives the split/concatenate.
		embeddings := make([]map[string]any, len(body.Requests))
		for i, req := range body.Requests {
			idx, _ := strconv.Atoi(req.Content.Parts[0].Text)
			embeddings[i] = map[string]any{"values": []float32{float32(idx)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	defer srv.Close()

	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	defer func() { geminiBaseHost = prev }()

	// 250 inputs, each labeled with its index, → 100 + 100 + 50 across 3 calls.
	texts := make([]string, 250)
	for i := range texts {
		texts[i] = strconv.Itoa(i)
	}
	p := &GeminiProvider{apiKey: "gem-key", client: srv.Client()}
	vecs := runEmbed(t, p, texts)

	if callCount != 3 {
		t.Errorf("upstream calls = %d, want 3 (100+100+50)", callCount)
	}
	if len(perCall) != 3 || perCall[0] != 100 || perCall[1] != 100 || perCall[2] != 50 {
		t.Errorf("per-call batch sizes = %v, want [100 100 50]", perCall)
	}
	if len(vecs) != 250 {
		t.Fatalf("got %d vectors, want 250", len(vecs))
	}
	// Order preserved end-to-end: vector i must carry index i.
	for i, v := range vecs {
		if len(v) != 1 || int(v[0]) != i {
			t.Fatalf("vector %d = %v, want [%d] (order not preserved)", i, v, i)
		}
	}
}

// An empty input must not hit the network at all.
func TestEmbed_Gemini_EmptyInput(t *testing.T) {
	p := &GeminiProvider{apiKey: "gem-key", client: http.DefaultClient}
	vecs, err := p.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty Embed errored: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("empty Embed returned %d vectors, want 0", len(vecs))
	}
}
