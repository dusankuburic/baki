package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"

	"pad-core/models"
)

// seedHotVariableFlow stores a flow where one flow-global variable has
// `n` writers and `n` readers: n*n data-flow edges each way unbounded.
func seedHotVariableFlow(t *testing.T, rt *Router, id, ownerID string, n int) {
	t.Helper()
	var blocks []models.Block
	for i := 0; i < n; i++ {
		blocks = append(blocks,
			models.Block{ID: fmt.Sprintf("w%d", i), Name: "Set", Type: models.BlockTypeAction, RawType: "Variables.SetVariable",
				SubflowID: "Main", LineNumber: 2*i + 1, Properties: map[string]string{"_output": "Hot"}},
			models.Block{ID: fmt.Sprintf("r%d", i), Name: "Show", Type: models.BlockTypeAction, RawType: "Display.ShowMessageDialog",
				SubflowID: "Main", LineNumber: 2*i + 2, Variables: []string{"Hot"}, Properties: map[string]string{"Message": "%Hot%"}},
		)
	}
	content, err := json.Marshal(models.FlowDocument{ID: id, Name: "hot", Subflows: []models.Subflow{{ID: "Main", Name: "Main", Blocks: blocks}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID: id, Name: "hot", OwnerID: ownerID, Content: json.RawMessage(content),
	}); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

// TestHandleGetDataFlow_BoundedWithTruncationReport: the endpoint answers in
// the old shape plus truncated/limits, and a hot variable's edges are capped
// per variable instead of listing every writer for every reader.
func TestHandleGetDataFlow_BoundedWithTruncationReport(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedHotVariableFlow(t, rt, "flow-hot", "alice", 300)
	tok := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/dataflow", tok, map[string]any{"flowId": "flow-hot"})
	checkStatus(t, rr, http.StatusOK)
	var res struct {
		Blocks map[string]struct {
			Reads            []string `json:"reads"`
			UpstreamBlocks   []string `json:"upstreamBlocks"`
			DownstreamBlocks []string `json:"downstreamBlocks"`
		} `json:"blocks"`
		TaintPaths []any `json:"taintPaths"`
		DeadData   []any `json:"deadData"`
		Truncated  bool  `json:"truncated"`
		Limits     struct {
			MaxEdgesPerVariable int `json:"maxEdgesPerVariable"`
		} `json:"limits"`
	}
	decodeJSON(t, rr, &res)
	if len(res.Blocks) != 600 {
		t.Fatalf("got %d blocks, want 600", len(res.Blocks))
	}
	if !res.Truncated || res.Limits.MaxEdgesPerVariable <= 0 {
		t.Fatalf("truncated=%v limits=%+v, want a truncation report", res.Truncated, res.Limits)
	}
	for id, b := range res.Blocks {
		if len(b.UpstreamBlocks) > res.Limits.MaxEdgesPerVariable || len(b.DownstreamBlocks) > res.Limits.MaxEdgesPerVariable {
			t.Fatalf("block %s: %d upstream, %d downstream, per-variable limit %d",
				id, len(b.UpstreamBlocks), len(b.DownstreamBlocks), res.Limits.MaxEdgesPerVariable)
		}
	}
}

// TestHandleGetDataFlow_CancelledRequestStops: the analysis runs on the
// request context, so a request whose context is already done gets an error
// instead of a full analysis nobody will read.
func TestHandleGetDataFlow_CancelledRequestStops(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedHotVariableFlow(t, rt, "flow-hot", "alice", 10)
	tok := jwtBearer(t, rt, "alice", "alice@example.com")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/analysis/dataflow", strings.NewReader(`{"flowId":"flow-hot"}`)).WithContext(ctx)
	req.Header.Set("Authorization", tok)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("cancelled request answered 200 with %d bytes", rr.Body.Len())
	}
}
