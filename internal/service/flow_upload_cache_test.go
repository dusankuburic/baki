package service

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/testutil"

	"pad-core/cache"
)

const uploadFlowText = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: $'''https://api.example.com/x'''\n" +
	"#EndRegion\n"

// TestLoadFlowFiles_CacheIsScopedToTheCaller is the regression gate for a cross-tenant
// write: LoadFlowFiles cached parsed docs in a process-global LRU keyed only by
// sha256(sorted filenames ‖ contents), so byte-identical uploads (a shared vendor
// template, the same PAD export circulating in a team) collided across users — the
// cached doc carries its ID, and the second uploader's SaveUploadedFlow upserted against
// the first's row (bumping the victim's OCC version, overwriting their name/source on
// every upload; the victim's own edits then failed 409 forever). The cache-hit comment
// already said "LoadFlowFiles is a desktop-only code path"; POST /api/flow/upload is
// exactly that JWT/cloud handler.
func TestLoadFlowFiles_CacheIsScopedToTheCaller(t *testing.T) {
	astCache, _ := cache.NewLRUCache(100)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	files := map[string]string{"Main.txt": uploadFlowText}

	alice, err := svc.LoadFlowFiles(ctx, files, "Alice Payroll Automation", "alice")
	if err != nil {
		t.Fatalf("alice upload: %v", err)
	}
	// Byte-identical content, different user, different flow name.
	bob, err := svc.LoadFlowFiles(ctx, files, "Bob Unrelated Name", "bob")
	if err != nil {
		t.Fatalf("bob upload: %v", err)
	}

	if bob.ID == alice.ID {
		t.Errorf("bob's upload was served alice's cached document (id %s) — saving it upserts "+
			"HER flow row: her version is bumped, her name and source overwritten, and bob's "+
			"flow is never created", alice.ID)
	}
	if bob.Name == alice.Name {
		t.Errorf("bob's document carries alice's flow name %q, which he never supplied", alice.Name)
	}
}

// TestLoadFlowFiles_SameCallerStillHitsTheCache keeps the cache doing its job.
// Scoping by caller must not turn it into a no-op, or this is a performance
// regression dressed as a security fix.
func TestLoadFlowFiles_SameCallerStillHitsTheCache(t *testing.T) {
	astCache, _ := cache.NewLRUCache(100)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	files := map[string]string{"Main.txt": uploadFlowText}

	first, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("the same caller re-uploading identical content missed the cache "+
			"(%s vs %s) — the cache no longer serves anyone", first.ID, second.ID)
	}
}

// TestLoadFlowFiles_AnonymousCallersDoNotShareEntries covers the desktop/local
// shape, where there is no JWT and so no caller id. Two empty-id callers are
// the same local user, so sharing is correct there — but an empty id must not
// become a wildcard that also matches an identified caller's entry.
func TestLoadFlowFiles_AnonymousCallersDoNotShareEntries(t *testing.T) {
	astCache, _ := cache.NewLRUCache(100)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	files := map[string]string{"Main.txt": uploadFlowText}

	local, err := svc.LoadFlowFiles(ctx, files, "Local", "")
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	identified, err := svc.LoadFlowFiles(ctx, files, "Cloud", "alice")
	if err != nil {
		t.Fatalf("identified: %v", err)
	}
	if identified.ID == local.ID {
		t.Error("an identified caller was served the anonymous/local cache entry")
	}
}

// targetingNotifierStub distinguishes broadcast from targeted delivery.
// testutil.CountingNotifier cannot: its EmitTo delegates straight to Emit and
// says so ("does not distinguish per-user delivery"), so asserting through it
// would pass whether or not the fix is present.
type targetingNotifierStub struct {
	mu            sync.Mutex
	broadcasts    []string
	broadcastData map[string][]any
	targeted      map[string][]string
	// payloads records (event, data) for targeted deliveries so tests can
	// assert on the payload shape, not just the count.
	targetedPayloads map[string][]any
}

func newTargetingNotifierStub() *targetingNotifierStub {
	return &targetingNotifierStub{
		broadcastData:    map[string][]any{},
		targeted:         map[string][]string{},
		targetedPayloads: map[string][]any{},
	}
}

func (n *targetingNotifierStub) EmitToOrg(_ context.Context, orgID, name string, data any) {
	n.EmitTo("org:"+orgID, name, data)
}

func (n *targetingNotifierStub) EmitToAll(name string, data any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.broadcasts = append(n.broadcasts, name)
	n.broadcastData[name] = append(n.broadcastData[name], data)
}

func (n *targetingNotifierStub) EmitTo(userID, name string, data any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.targeted[userID] = append(n.targeted[userID], name)
	n.targetedPayloads[userID+"|"+name] = append(n.targetedPayloads[userID+"|"+name], data)
}

func (n *targetingNotifierStub) HasSubscriber(string) bool { return true }

func (n *targetingNotifierStub) countBroadcast(name string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, e := range n.broadcasts {
		if e == name {
			c++
		}
	}
	return c
}

func (n *targetingNotifierStub) countTargeted(userID, name string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, e := range n.targeted[userID] {
		if e == name {
			c++
		}
	}
	return c
}

// targetedPayload returns the data payloads of the targeted (userID, name)
// deliveries, for shape assertions.
func (n *targetingNotifierStub) targetedPayload(userID, name string) []any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]any{}, n.targetedPayloads[userID+"|"+name]...)
}

// TestLoadFlowFiles_LoadedEventIsAddressedToTheCaller is the second half of the
// same defect. The cache fix stops one tenant's document being SAVED over
// another's; this stops it being BROADCAST to them.
//
// A broadcast (EmitToAll, then the bare Emit) delivers to every connected SSE
// client, and LoadFlowFiles emitted the whole parsed document through it — so in cloud
// mode every upload shipped its full contents, block tree and all, to every
// other tenant with an open event stream. No collision or attacker needed; it
// happened on every single upload.
func TestLoadFlowFiles_LoadedEventIsAddressedToTheCaller(t *testing.T) {
	astCache, _ := cache.NewLRUCache(100)
	notifier := newTargetingNotifierStub()
	svc := NewFlowService(notifier, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	files := map[string]string{"Main.txt": uploadFlowText}
	if _, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// The second call takes the cache-HIT branch, which emits separately and
	// carried the same defect.
	if _, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice"); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	if n := notifier.countBroadcast("flow:loaded"); n != 0 {
		t.Errorf("flow:loaded was broadcast to ALL clients %d time(s) — each one delivers the "+
			"full parsed document to every other tenant's event stream", n)
	}
	if n := notifier.countTargeted("alice", "flow:loaded"); n != 2 {
		t.Errorf("caller received %d targeted flow:loaded events, want 2 (miss path + cache-hit path)", n)
	}
}

// TestLoadFlowFiles_LoadedEventCarriesOnlyASummary pins the flow:loaded SSE
// payload contract. The event must carry the lightweight FlowLoadedSummary —
// never the FlowDocument — because the SSE hub pins every queued frame per
// connected client: a document-sized event (Source + Subflows, hundreds of MB
// for large flows) turns one upload into gigabytes of pinned memory across a
// few stalled connections. Both the parse (miss) and cache-hit emission paths
// are covered.
func TestLoadFlowFiles_LoadedEventCarriesOnlyASummary(t *testing.T) {
	astCache, _ := cache.NewLRUCache(100)
	notifier := newTargetingNotifierStub()
	svc := NewFlowService(notifier, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	files := map[string]string{"Main.txt": uploadFlowText}
	doc, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice"); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	payloads := notifier.targetedPayload("alice", "flow:loaded")
	if len(payloads) != 2 {
		t.Fatalf("captured %d flow:loaded payloads, want 2", len(payloads))
	}
	for i, p := range payloads {
		sum, ok := p.(FlowLoadedSummary)
		if !ok {
			t.Fatalf("payload %d is %T, want FlowLoadedSummary — the full document must never ride the SSE stream", i, p)
		}
		if sum.ID != doc.ID {
			t.Errorf("payload %d id = %q, want the uploaded flow's id %q", i, sum.ID, doc.ID)
		}
		if sum.Name != doc.Name {
			t.Errorf("payload %d name = %q, want %q", i, sum.Name, doc.Name)
		}
		if sum.FilePath != "" {
			t.Errorf("payload %d carries filePath %q — uploads have no on-disk source; consumers must refetch from the library", i, sum.FilePath)
		}
		if sum.IsFolder {
			t.Errorf("payload %d claims a folder flow for a single-file upload", i)
		}
	}
}
