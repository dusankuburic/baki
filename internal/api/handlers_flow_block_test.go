package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"

	"pad-core/models"
)

// blockEditSource is a flow with two sibling actions inside a region, which is
// the minimum shape the block endpoints need: something to remove, duplicate,
// rename, reorder and re-parent.
const blockEditSource = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/a'\n" +
	"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/b'\n" +
	"#EndRegion\n"

func newBlockEditRouter(t *testing.T) (*Router, string, *models.FlowDocument) {
	t.Helper()
	rt := newTestRouter(&testutil.FakeBackend{}, true)
	seedUserWithRole(t, rt, "editor", "editor@example.com", auth.RoleMember)
	doc := seedCloudFlowWithSource(t, rt, "flow-blocks", "editor", blockEditSource)
	return rt, "flow-blocks", doc
}

// actionBlockIDs returns the ids of the HTTP actions in the doc, in source
// order. The fixture has two, so callers get both a target and a sibling to
// reference.
func actionBlockIDs(t *testing.T, doc *models.FlowDocument) []string {
	t.Helper()
	var ids []string
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if blocks[i].RawType == "HTTPClient.InvokeUrl" {
				ids = append(ids, blocks[i].ID)
			}
			walk(blocks[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	if len(ids) < 2 {
		t.Fatalf("fixture must contain 2 HTTP actions, found %d", len(ids))
	}
	return ids
}

// firstActionBlockID is the single-target shorthand.
func firstActionBlockID(t *testing.T, doc *models.FlowDocument) string {
	t.Helper()
	return actionBlockIDs(t, doc)[0]
}

// blockEndpoints is every route under /api/flow/block/*, with a body that is
// valid except for the field each case is probing.
var blockEndpoints = []struct {
	name string
	path string
	body map[string]any // blockId filled in per-test
}{
	{"remove", "/api/flow/block/remove", map[string]any{}},
	{"duplicate", "/api/flow/block/duplicate", map[string]any{}},
	{"properties", "/api/flow/block/properties", map[string]any{"changes": map[string]string{"Url": "'https://x'"}}},
	{"move", "/api/flow/block/move", map[string]any{"direction": "down"}},
	{"move-to", "/api/flow/block/move-to", map[string]any{"refBlockId": "REF", "position": "after"}},
	{"rename", "/api/flow/block/rename", map[string]any{"name": "Renamed"}},
	{"remove-batch", "/api/flow/block/remove-batch", map[string]any{}},
}

// TestBlockEndpoints_RequireEditor is the authz gate for the visual editor's
// mutation surface. Every one of these rewrites the flow source, so a viewer
// must be refused by all of them — a single unguarded route is enough to let a
// read-only collaborator edit.
func TestBlockEndpoints_RequireEditor(t *testing.T) {
	rt, flowID, doc := newBlockEditRouter(t)
	ids := actionBlockIDs(t, doc)
	blockID, siblingID := ids[0], ids[1]
	seedUserWithRole(t, rt, "viewer", "viewer@example.com", auth.RoleMember)
	viewer := jwtBearer(t, rt, "viewer", "viewer@example.com")

	for _, ep := range blockEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			body := map[string]any{"flowId": flowID, "blockId": blockID, "blockIds": []string{blockID}}
			for k, v := range ep.body {
				if v == "REF" {
					v = siblingID
				}
				body[k] = v
			}
			rr := doRequestWithAuth(t, rt, http.MethodPost, ep.path, viewer, body)
			checkStatus(t, rr, http.StatusForbidden)
		})
	}
}

// TestBlockEndpoints_RejectMissingBlockID pins the validation contract: a
// missing target is a 400 (the client sent a bad request), not a 500 and not a
// silent no-op that returns 200 and leaves the user thinking it worked.
//
// Note the ordering this documents: these handlers validate the body BEFORE
// resolving the flow, so a malformed request is answered the same way whether
// or not the caller may edit the flow. That leaks nothing (the answer depends
// only on the body the caller sent) and it is why the authz test above has to
// send a well-formed body to see a 403.
func TestBlockEndpoints_RejectMissingBlockID(t *testing.T) {
	rt, flowID, _ := newBlockEditRouter(t)
	bearer := jwtBearer(t, rt, "editor", "editor@example.com")

	for _, ep := range blockEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			body := map[string]any{"flowId": flowID}
			for k, v := range ep.body {
				body[k] = v
			}
			rr := doRequestWithAuth(t, rt, http.MethodPost, ep.path, bearer, body)
			checkStatus(t, rr, http.StatusBadRequest)
		})
	}
}

// TestBlockEndpoints_RejectMalformedBody guards the decode path on every
// route — a malformed body must be a 400, never a panic recovered into a 500.
func TestBlockEndpoints_RejectMalformedBody(t *testing.T) {
	rt, _, _ := newBlockEditRouter(t)
	for _, ep := range blockEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			rr := newBadBodyRequest(t, rt, http.MethodPost, ep.path)
			if rr.Code != http.StatusBadRequest && rr.Code != http.StatusUnauthorized {
				t.Errorf("malformed body: got %d, want 400 (or 401 before auth)", rr.Code)
			}
		})
	}
}

// TestBlockRemove_RemovesTheBlock is the happy path: the editor's delete
// reaches the source and the returned document no longer contains the block.
func TestBlockRemove_RemovesTheBlock(t *testing.T) {
	rt, flowID, doc := newBlockEditRouter(t)
	bearer := jwtBearer(t, rt, "editor", "editor@example.com")
	blockID := firstActionBlockID(t, doc)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/block/remove", bearer,
		map[string]any{"flowId": flowID, "blockId": blockID})
	checkStatus(t, rr, http.StatusOK)

	var res struct {
		Document *models.FlowDocument `json:"document"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Document == nil {
		t.Fatal("response carried no document")
	}
	// The re-parse mints fresh block ids, so count actions rather than ids.
	if got := countActions(res.Document); got != 1 {
		t.Errorf("expected 1 remaining HTTP action after removing one of two, got %d", got)
	}
}

// TestBlockDuplicate_AddsACopy is the mirror of the remove happy path.
func TestBlockDuplicate_AddsACopy(t *testing.T) {
	rt, flowID, doc := newBlockEditRouter(t)
	bearer := jwtBearer(t, rt, "editor", "editor@example.com")
	blockID := firstActionBlockID(t, doc)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/block/duplicate", bearer,
		map[string]any{"flowId": flowID, "blockId": blockID})
	checkStatus(t, rr, http.StatusOK)

	var res struct {
		Document *models.FlowDocument `json:"document"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := countActions(res.Document); got != 3 {
		t.Errorf("expected 3 HTTP actions after duplicating one of two, got %d", got)
	}
}

// TestBlockEndpoints_UnknownBlockIsNotFoundOrConflict: a stale block id (the
// UI holding an id from before someone else's edit) must be a clean client
// error, not a 500.
func TestBlockEndpoints_UnknownBlockIsNotFoundOrConflict(t *testing.T) {
	rt, flowID, _ := newBlockEditRouter(t)
	bearer := jwtBearer(t, rt, "editor", "editor@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/block/remove", bearer,
		map[string]any{"flowId": flowID, "blockId": "no-such-block"})
	if rr.Code < 400 || rr.Code >= 500 {
		t.Errorf("unknown block id: got %d, want a 4xx", rr.Code)
	}
}

func countActions(doc *models.FlowDocument) int {
	n := 0
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			if blocks[i].RawType == "HTTPClient.InvokeUrl" {
				n++
			}
			walk(blocks[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	return n
}
