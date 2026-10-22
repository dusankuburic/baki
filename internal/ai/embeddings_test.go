package ai

import (
	"context"
	"encoding/json"
	"log/slog"
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

// geminiEmbedRequest is one entry of a recorded batchEmbedContents body.
type geminiEmbedRequest struct {
	Model                string `json:"model"`
	TaskType             string `json:"taskType"`
	OutputDimensionality int    `json:"outputDimensionality"`
}

// geminiEmbedServer points the Gemini provider at a fake batchEmbedContents that records every request and
// answers vectors of the requested width (2 wide when none is requested, as a model answering at its native
// width would).
func geminiEmbedServer(t *testing.T) (*http.Client, *[]geminiEmbedRequest) {
	t.Helper()
	var seen []geminiEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Requests []geminiEmbedRequest `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Requests...)
		embeddings := make([]map[string]any, len(body.Requests))
		for i, req := range body.Requests {
			width := req.OutputDimensionality
			if width == 0 {
				width = 2
			}
			embeddings[i] = map[string]any{"values": make([]float32, width)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	t.Cleanup(func() {
		srv.Close()
		geminiBaseHost = prev
	})
	return srv.Client(), &seen
}

// TestEmbed_Gemini_FactoryWidthReachesEveryBatch (R-knowledge-4): the knowledge base's configured width
// is sent as outputDimensionality on every batchEmbedContents request, so the vectors come back at the
// width Postgres indexes — not at the model's native 3072.
func TestEmbed_Gemini_FactoryWidthReachesEveryBatch(t *testing.T) {
	_, seen := geminiEmbedServer(t)
	f := NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	f.SetEmbeddingDimensions(1536)
	p, err := f.ForEmbedding(context.Background(), "", "gemini", "")
	if err != nil {
		t.Fatal(err)
	}
	// 101 texts: two batchEmbedContents requests.
	texts := make([]string, 101)
	for i := range texts {
		texts[i] = strconv.Itoa(i)
	}
	vecs := runEmbed(t, p, texts)
	if len(*seen) != 101 {
		t.Fatalf("recorded %d requests, want 101", len(*seen))
	}
	for i, req := range *seen {
		if req.OutputDimensionality != 1536 {
			t.Fatalf("request %d outputDimensionality = %d, want 1536", i, req.OutputDimensionality)
		}
	}
	for i, v := range vecs {
		if len(v) != 1536 {
			t.Fatalf("vector %d is %d wide, want 1536", i, len(v))
		}
	}
	if got, want := EmbeddingIdentity(p), "gemini|"+geminiEmbedModelDefault; got != want {
		t.Errorf("identity = %q, want %q (the width is not part of the frozen gemini identity, ADR 0027)", got, want)
	}
}

// TestGeminiEmbeddingDimensionsFor (R-knowledge-4): outputDimensionality can only shorten a vector, so a
// width above the model's native width is not requested; a model whose width is unknown gets the
// configured width as is.
func TestGeminiEmbeddingDimensionsFor(t *testing.T) {
	cases := []struct {
		model string
		n     int
		want  int
	}{
		{"gemini-embedding-001", 1536, 1536},
		{"gemini-embedding-001", 3072, 3072},
		{"gemini-embedding-001", 4096, 0},
		{"gemini-embedding-exp-03-07", 768, 768},
		{"text-embedding-004", 768, 768},
		{"text-embedding-004", 1536, 0},
		{"gemini-embedding-001", 0, 0},
		{"some-future-model", 1024, 1024},
	}
	for _, tc := range cases {
		if got := geminiEmbeddingDimensionsFor(tc.model, tc.n); got != tc.want {
			t.Errorf("geminiEmbeddingDimensionsFor(%q, %d) = %d, want %d", tc.model, tc.n, got, tc.want)
		}
	}
}

// warnProbe counts the slog records whose message contains needle.
type warnProbe struct {
	needle string
	n      int
}

func (p *warnProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p *warnProbe) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, p.needle) {
		p.n++
	}
	return nil
}
func (p *warnProbe) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p *warnProbe) WithGroup(string) slog.Handler      { return p }

// TestEmbed_Gemini_WidthAboveNativeFallsBackAndLogsOnce (R-knowledge-4): a configured width the model
// cannot produce is not sent (the call embeds at the native width, which the knowledge base will then
// report as a width mismatch) and the misconfiguration is logged once per process, not per batch.
func TestEmbed_Gemini_WidthAboveNativeFallsBackAndLogsOnce(t *testing.T) {
	client, seen := geminiEmbedServer(t)
	resetGeminiWidthWarning()
	probe := &warnProbe{needle: "native width"}
	prev := slog.Default()
	slog.SetDefault(slog.New(probe))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := &GeminiProvider{apiKey: "k", client: client}
	p.setEmbeddingDimensions(4096)
	runEmbed(t, p, []string{"a"})
	// A second provider instance at the same model and width (the factory builds one per request).
	q := &GeminiProvider{apiKey: "k", client: client}
	q.setEmbeddingDimensions(4096)
	runEmbed(t, q, []string{"b"})

	if len(*seen) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(*seen))
	}
	for i, req := range *seen {
		if req.OutputDimensionality != 0 {
			t.Errorf("request %d sent outputDimensionality %d above gemini-embedding-001's native 3072", i, req.OutputDimensionality)
		}
	}
	if probe.n != 1 {
		t.Errorf("the above-native width was logged %d times, want once", probe.n)
	}
}

// TestEmbed_Gemini_TaskTypeByEmbeddingUse (R-knowledge-4): a search query is embedded as
// RETRIEVAL_QUERY and a stored document as RETRIEVAL_DOCUMENT — Gemini's asymmetric retrieval pairing.
func TestEmbed_Gemini_TaskTypeByEmbeddingUse(t *testing.T) {
	client, seen := geminiEmbedServer(t)
	p := &GeminiProvider{apiKey: "k", client: client}
	runEmbed(t, p, []string{"a chunk"})
	if _, err := p.Embed(WithQueryEmbedding(context.Background()), []string{"a question"}); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(*seen))
	}
	if got := (*seen)[0].TaskType; got != "RETRIEVAL_DOCUMENT" {
		t.Errorf("document embed taskType = %q, want RETRIEVAL_DOCUMENT", got)
	}
	if got := (*seen)[1].TaskType; got != "RETRIEVAL_QUERY" {
		t.Errorf("query embed taskType = %q, want RETRIEVAL_QUERY", got)
	}
	if IsQueryEmbedding(context.Background()) || !IsQueryEmbedding(WithQueryEmbedding(context.Background())) {
		t.Error("IsQueryEmbedding does not read WithQueryEmbedding")
	}
}

// Gemini's batchEmbedContents caps inputs per call, but callers embed up to
// maxKnowledgeChunks (2500) at once. Embed must split into ≤100-item sub-batches
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

// TestSetEmbeddingModel_OverrideChangesRequestModel proves the override reaches
// the actual /embeddings request — the core of the "decouple embedding model
// from provider" feature (a deployer picks e.g. text-embedding-3-large without
// code changes).
func TestSetEmbeddingModel_OverrideChangesRequestModel(t *testing.T) {
	srv, got := embedTestServer(t, "") // no static assertion — we make two calls with different models
	defer srv.Close()
	base := srv.URL + "/chat/completions"
	p := &OpenAIProvider{openaiBase: openaiBase{
		apiKey: "sk-test", client: srv.Client(), baseURL: &base,
		providerLabel: "openai", embeddingModel: "text-embedding-3-small",
		embeddingModelDefault: "text-embedding-3-small",
	}}

	p.setEmbeddingModel("text-embedding-3-large")
	runEmbed(t, p, []string{"x"})
	if (*got)["model"] != "text-embedding-3-large" {
		t.Errorf("override did not take effect: model = %v", (*got)["model"])
	}

	// An empty model restores the default.
	p.setEmbeddingModel("")
	runEmbed(t, p, []string{"y"})
	if (*got)["model"] != "text-embedding-3-small" {
		t.Errorf("empty override should restore default: model = %v", (*got)["model"])
	}
}

// TestForEmbedding_AppliesOverride proves the factory threads the override
// through the wrap chain to the underlying provider.
func TestForEmbedding_AppliesOverride(t *testing.T) {
	srv, got := embedTestServer(t, "custom-embed-model")
	defer srv.Close()

	orig := openAIBaseURL
	openAIBaseURL = srv.URL + "/chat/completions"
	defer func() { openAIBaseURL = orig }()

	f := NewProviderFactory(func(scope, provider string) (string, error) { return "sk-test", nil }, nil, nil, nil)
	p, err := f.ForEmbedding(context.Background(), "", "openai", "custom-embed-model")
	if err != nil {
		t.Fatalf("ForEmbedding: %v", err)
	}
	runEmbed(t, p, []string{"hello"})
	if (*got)["model"] != "custom-embed-model" {
		t.Errorf("ForEmbedding override did not reach the request: model = %v", (*got)["model"])
	}
}

// TestForEmbedding_NoOverrideUsesDefault proves an empty override leaves the
// provider on its shipped default model.
func TestForEmbedding_NoOverrideUsesDefault(t *testing.T) {
	srv, got := embedTestServer(t, "")
	defer srv.Close()
	orig := openAIBaseURL
	openAIBaseURL = srv.URL + "/chat/completions"
	defer func() { openAIBaseURL = orig }()

	f := NewProviderFactory(func(scope, provider string) (string, error) { return "sk-test", nil }, nil, nil, nil)
	p, err := f.ForEmbedding(context.Background(), "", "openai", "")
	if err != nil {
		t.Fatalf("ForEmbedding: %v", err)
	}
	runEmbed(t, p, []string{"hello"})
	if (*got)["model"] != "text-embedding-3-small" {
		t.Errorf("expected default model, got %v", (*got)["model"])
	}
}
