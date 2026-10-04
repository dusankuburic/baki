package ai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAzureFoundry_EmbedDimensionsRefusalRememberedAcrossInstances (D-10 /
// V-rag #4): the `dimensions` refusal is memoised per endpoint + model, so a
// fresh provider (the factory builds one per request) pays no second
// rejection round trip; another model on the same endpoint still learns.
func TestAzureFoundry_EmbedDimensionsRefusalRememberedAcrossInstances(t *testing.T) {
	srv := azureEmbedServer(t, rejectDimensions) // resets the memo around the test
	cfg := azureKeyConfig(srv.URL)
	cfg.EmbeddingDeployment = "kb-embed"

	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	p.setEmbeddingDimensions(1536)
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("first instance: %d requests, want 2 (the refusal is learned once)", n)
	}

	// A second, fresh instance on the same endpoint + model: no repair.
	q := newAzureFoundryProvider(cfg, nil, srv.Client())
	q.setEmbeddingDimensions(1536)
	if _, err := q.Embed(context.Background(), []string{"c"}); err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("second instance: %d requests in total, want 3 (one, already repaired)", len(reqs))
	}
	if _, sent := reqs[2].Body["dimensions"]; sent {
		t.Errorf("the remembered refusal was not applied: %v", reqs[2].Body)
	}

	// Another deployment on the same endpoint is a different model: it
	// learns its own refusal.
	other := cfg
	other.EmbeddingDeployment = "kb-embed-2"
	r := newAzureFoundryProvider(other, nil, srv.Client())
	r.setEmbeddingDimensions(1536)
	if _, err := r.Embed(context.Background(), []string{"d"}); err != nil {
		t.Fatalf("other deployment Embed: %v", err)
	}
	reqs = srv.requests()
	if len(reqs) != 5 || reqs[3].Body["dimensions"] != float64(1536) {
		t.Errorf("other deployment must send dimensions once: %d requests, third body %v", len(reqs), reqs[3].Body)
	}

	resetEmbedDimsRefusals()
	if p.dimensionsRefused() {
		t.Error("resetEmbedDimsRefusals must clear the memo")
	}
}

// TestEmbed_SplitCascadeDoesNotRelearnDimensions (V-rag-1): after the parent
// batch's refusal, the split sub-batches go out without `dimensions` — one
// refusal per cascade, not one per leaf.
func TestEmbed_SplitCascadeDoesNotRelearnDimensions(t *testing.T) {
	resetEmbedDimsRefusals()
	t.Cleanup(resetEmbedDimsRefusals)
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, body map[string]any) {
		if _, sent := body["dimensions"]; sent {
			rejectDimensions(w)
			return
		}
		inputs, _ := body["input"].([]any)
		if len(inputs) > 1 {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "Too many inputs in a single request. The max number of inputs is 1.", "code": "BadRequest"}})
			return
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"embedding": []float32{0.5}}}})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "kb-embed" })
	p.setEmbeddingDimensions(1536)

	vecs, err := p.Embed(context.Background(), []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 4 {
		t.Fatalf("got %d vectors, want 4", len(vecs))
	}
	refusals := 0
	for _, r := range srv.requests() {
		if _, sent := r.Body["dimensions"]; sent {
			refusals++
		}
	}
	if refusals != 1 {
		t.Errorf("%d requests carried dimensions, want exactly 1 (the cascade must not relearn)", refusals)
	}
}

func embedResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
}

// TestDecodeEmbeddings_PlacesByIndex (D-46): vectors are placed by `index`
// when every item carries one; a permutation, a duplicate, a short answer
// and an out-of-range index are handled; an index-less answer stays
// positional.
func TestDecodeEmbeddings_PlacesByIndex(t *testing.T) {
	vecs, err := decodeEmbeddings(embedResponse(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`), 2)
	if err != nil {
		t.Fatalf("permuted: %v", err)
	}
	if vecs[0][0] != 1 || vecs[0][1] != 0 || vecs[1][0] != 0 || vecs[1][1] != 1 {
		t.Errorf("permuted answer not placed by index: %v", vecs)
	}

	vecs, err = decodeEmbeddings(embedResponse(`{"data":[{"embedding":[0,1]},{"embedding":[1,0]}]}`), 2)
	if err != nil || vecs[0][0] != 0 || vecs[1][0] != 1 {
		t.Errorf("index-less answer must stay positional: %v %v", vecs, err)
	}

	for name, body := range map[string]string{
		"duplicate index": `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`,
		"short answer":    `{"data":[{"index":0,"embedding":[1]}]}`,
		"out of range":    `{"data":[{"index":0,"embedding":[1]},{"index":5,"embedding":[2]}]}`,
		"negative index":  `{"data":[{"index":-1,"embedding":[1]},{"index":0,"embedding":[2]}]}`,
	} {
		if _, err := decodeEmbeddings(embedResponse(body), 2); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
