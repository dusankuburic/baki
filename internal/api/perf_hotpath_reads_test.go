package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Tests for the 2026-10 performance round (HTTP side).

// readCountingBackend wraps the filesystem backend and counts the reads the
// round removed from hot paths.
type readCountingBackend struct {
	*filesystem.LocalStorageBackend
	loadFlow       int
	listStatuses   int
	getStatus      int
	recordedAlerts atomic.Int64
}

func (b *readCountingBackend) LoadFlow(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	b.loadFlow++
	return b.LocalStorageBackend.LoadFlow(ctx, id)
}

func (b *readCountingBackend) ListFindingStatuses(ctx context.Context, flowID string) ([]*storageif.FindingStatus, error) {
	b.listStatuses++
	return b.LocalStorageBackend.ListFindingStatuses(ctx, flowID)
}

func (b *readCountingBackend) GetFindingStatus(ctx context.Context, flowID, findingKey string) (*storageif.FindingStatus, error) {
	b.getStatus++
	return b.LocalStorageBackend.GetFindingStatus(ctx, flowID, findingKey)
}

func (b *readCountingBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	b.recordedAlerts.Add(1)
	return b.LocalStorageBackend.RecordGovernanceAlert(ctx, a)
}

func newReadCountingRouter(t *testing.T) (*Router, *readCountingBackend) {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}
	b := &readCountingBackend{LocalStorageBackend: fs}
	return newTestRouter(b, true), b
}

// TestLibraryGet_ReadsMetadataOnly: GET /api/library/{id} renders metadata
// (name, owner, tags, counts) and used to load the whole document for it.
func TestLibraryGet_ReadsMetadataOnly(t *testing.T) {
	rt, b := newReadCountingRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var out libraryFlow
	decodeJSON(t, rr, &out)
	if out.ID != "flow1" || out.Name != "t" || out.OwnerID != "alice" {
		t.Errorf("unexpected body: %+v", out)
	}
	if b.loadFlow != 0 {
		t.Errorf("GET /api/library/{id} loaded the full document %d times, want 0", b.loadFlow)
	}

	bob := jwtBearer(t, rt, "bob", "bob@example.com")
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1", bob, nil), http.StatusForbidden)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/nope", bearer, nil), http.StatusNotFound)
}

// TestTriageSet_ReadsHeaderAndOneStatus: the single-finding triage write
// resolved the whole flow document for its name and listed every status of
// the flow for one prior assignee.
func TestTriageSet_ReadsHeaderAndOneStatus(t *testing.T) {
	rt, b := newReadCountingRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	set := func(assignee string) {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
			"flowId": "flow1", "findingKey": "r1:b1", "ruleId": "r1", "status": "in_progress", "assigneeId": assignee,
		})
		checkStatus(t, rr, http.StatusOK)
	}
	set("bob")
	set("bob") // same assignee again: no second notification

	if b.loadFlow != 0 {
		t.Errorf("triage set loaded the full document %d times, want 0", b.loadFlow)
	}
	if b.listStatuses != 0 {
		t.Errorf("triage set listed the flow's statuses %d times, want 0", b.listStatuses)
	}
	if b.getStatus != 2 {
		t.Errorf("single-status lookups = %d, want 2", b.getStatus)
	}
	// No RLS transaction here (filesystem backend), so the post-commit hook
	// runs right after the handler; it only enqueues the fan-out, so wait for
	// the detached job: exactly one assignment alert, for the first set only.
	waitUntil(t, "assignment alert", func() bool { return b.recordedAlerts.Load() == 1 })

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set",
		jwtBearer(t, rt, "bob", "bob@example.com"), map[string]any{
			"flowId": "flow1", "findingKey": "r1:b1", "status": "resolved",
		}), http.StatusForbidden)
}

// TestTriageSet_AssignmentNoticeWaitsForCommit: the alert row and the
// display-name lookup ran synchronously inside the request on their own
// context.Background() connections — a second pooled connection while the
// RLS transaction held the first — and announced an assignment that the
// transaction could still roll back. They now run as a post-commit hook.
func TestTriageSet_AssignmentNoticeWaitsForCommit(t *testing.T) {
	rt, b := newReadCountingRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")

	body, _ := json.Marshal(map[string]any{
		"flowId": "flow1", "findingKey": "r1:b1", "ruleId": "r1", "status": "in_progress", "assigneeId": "bob",
	})
	// The registry the RLS middleware installs for a transactional request.
	ctx, postCommit := txhook.With(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/analysis/triage/set", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", jwtBearer(t, rt, "alice", "alice@example.com"))
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	checkStatus(t, rr, http.StatusOK)

	if n := b.recordedAlerts.Load(); n != 0 {
		t.Fatalf("assignment alert recorded before the commit (%d)", n)
	}
	postCommit.Commit(nil)
	waitUntil(t, "assignment alert after commit", func() bool { return b.recordedAlerts.Load() == 1 })
}

// patJoinBackend offers the one-round-trip token+owner lookup and fails the
// test if PAT authentication still takes the two-query path.
type patJoinBackend struct {
	*filesystem.LocalStorageBackend
	joined  int
	twoStep int
}

func (b *patJoinBackend) GetAPITokenWithOwnerByHash(_ context.Context, tokenHash string) (*storageif.APIToken, *storageif.User, error) {
	b.joined++
	if tokenHash != auth.HashAPIToken("raw-pat") {
		return nil, nil, storageif.ErrNotFound
	}
	exp := time.Now().Add(time.Hour)
	return &storageif.APIToken{ID: "pat-1", UserID: "u1", ExpiresAt: &exp, Scopes: []string{"read"}},
		&storageif.User{ID: "u1", Email: "u1@example.test", Role: auth.RoleMember}, nil
}

func (b *patJoinBackend) GetAPITokenByHash(ctx context.Context, tokenHash string) (*storageif.APIToken, error) {
	b.twoStep++
	return b.LocalStorageBackend.GetAPITokenByHash(ctx, tokenHash)
}

func (b *patJoinBackend) LoadUserByID(ctx context.Context, id string) (*storageif.User, error) {
	b.twoStep++
	return b.LocalStorageBackend.LoadUserByID(ctx, id)
}

// TestVerifyAPIToken_OneRoundTripWhenTheBackendJoins: PAT auth read the
// token and then its owner sequentially on every machine request.
func TestVerifyAPIToken_OneRoundTripWhenTheBackendJoins(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &patJoinBackend{LocalStorageBackend: fs}
	rt := newTestRouter(b, true)

	claims := rt.verifyAPIToken(context.Background(), "raw-pat")
	if claims == nil {
		t.Fatal("valid PAT rejected")
	}
	if claims.UserID != "u1" || claims.Role != auth.RoleMember {
		t.Errorf("claims = %+v", claims)
	}
	if b.joined != 1 || b.twoStep != 0 {
		t.Errorf("joined lookups = %d, two-step reads = %d; want 1 and 0", b.joined, b.twoStep)
	}
	if rt.verifyAPIToken(context.Background(), "unknown") != nil {
		t.Error("unknown PAT accepted")
	}
}

// TestLookupAPITokenOwner_FallbackStillChecksTheOwner: backends without the
// join keep the two-step path, including the "owner gone ⇒ reject" rule.
func TestLookupAPITokenOwner_FallbackStillChecksTheOwner(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.CreateAPIToken(ctx, &storageif.APIToken{ID: "pat-1", UserID: "ghost", Name: "ci", TokenHash: auth.HashAPIToken("raw")}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if tok, user := lookupAPITokenOwner(ctx, fs, auth.HashAPIToken("raw")); tok != nil || user != nil {
		t.Errorf("token of a missing owner resolved: %+v %+v", tok, user)
	}
}

// TestRLSExemptPath_OrgChannelTest: the org channel test dials a webhook for
// up to 15 s and must not hold the request transaction's pooled connection;
// its CRUD siblings keep their transaction.
func TestRLSExemptPath_OrgChannelTest(t *testing.T) {
	cases := map[string]bool{
		"/api/orgs/org-1/channels/ch-1/test":       true,
		"/api/orgs/org-1/channels/ch-1":            false,
		"/api/orgs/org-1/channels":                 false,
		"/api/orgs//channels/ch-1/test":            false,
		"/api/orgs/org-1/channels//test":           false,
		"/api/orgs/org-1/channels/ch-1/test/extra": false,
		"/api/orgs/org-1/x/channels/ch-1/test":     false,
	}
	for path, want := range cases {
		if got := rlsExemptPath(path); got != want {
			t.Errorf("rlsExemptPath(%q) = %v, want %v", path, got, want)
		}
	}
}
