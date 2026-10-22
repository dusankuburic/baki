package service

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// subscribedBus is a bus with the flow caches subscribed as di.SubscribeFlowCaches subscribes them, async
// deliveries inline.
func subscribedBus(flows *FlowService, dashboard *DashboardService, chat *ChatService) *eventbus.Bus {
	bus := eventbus.New(nil, time.Second)
	for _, s := range FlowCacheSubscribers(flows, dashboard, chat) {
		eventbus.Subscribe(bus, eventbus.Sync, s.Name, s.Fn)
	}
	return bus
}

// gatedDashStore counts home builds; while gate is set, a build signals entered and waits for gate.
type gatedDashStore struct {
	*testutil.FakeBackend
	builds  atomic.Int32
	gate    chan struct{}
	entered chan struct{}
}

func (s *gatedDashStore) FlowDashboardData(context.Context, string, int) (*storageif.DashboardData, error) {
	s.builds.Add(1)
	if s.gate != nil {
		s.entered <- struct{}{}
		<-s.gate
	}
	return &storageif.DashboardData{}, nil
}

func (s *gatedDashStore) FlowDashboardAdvanced(context.Context, string, int) (*storageif.DashboardAdvancedData, error) {
	return &storageif.DashboardAdvancedData{}, nil
}

const eventsTestSrc = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://a''' Method: HTTPClient.Method.GET\n#EndRegion\n"

// The plan's verification: a library update and a flow save (the editor's path) drop the same caches — the
// owner's dashboard, the chat's scrubbed context and the search index — through the same FlowChanged.
func TestFlowCaches_LibraryAndFlowMutationsInvalidateTheSameCaches(t *testing.T) {
	scrubs := countScrubs(t)
	backend := testutil.NewFakeBackend()
	authz := NewAuthzService(backend, nil)
	flows := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, authz, nil)
	store := &gatedDashStore{FakeBackend: backend}
	dashboard := NewDashboardService(store, nil, flows, nil)
	chat := &ChatService{chatCtxCache: newChatContextCache()}
	bus := subscribedBus(flows, dashboard, chat)
	flows.bus = bus
	lib := NewLibraryService(backend, bus, authz, nil)
	ctx := actorCtx("u1")

	up, err := parser.ParseText(eventsTestSrc, "Main.txt", int64(len(eventsTestSrc)))
	if err != nil {
		t.Fatal(err)
	}
	up.Source = eventsTestSrc
	if err := flows.SaveUploadedFlow(ctx, up, "u1"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	id := up.ID
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}

	// fill builds each cache from the stored flow and reports the document it used.
	fill := func() *models.FlowDocument {
		t.Helper()
		doc, err := flows.DocProvider().ResolveDoc(ctx, id)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		dashboard.BuildHome(ctx, "u1")
		chat.cachedContextCore(ctx, "u1", ctxCacheStubProvider{}, doc, nil, req)
		flows.searchIndexFor(doc)
		return doc
	}
	// dropped asserts that each cache misses after the mutation named what.
	dropped := func(what string, doc *models.FlowDocument) {
		t.Helper()
		builds, scrubbed := store.builds.Load(), scrubs.Load()
		if _, ok := flows.idxCache.Get(searchIndexKey(t, id)); ok {
			t.Errorf("%s: the search index survived", what)
		}
		dashboard.BuildHome(ctx, "u1")
		if store.builds.Load() != builds+1 {
			t.Errorf("%s: the owner's dashboard was served from the cache", what)
		}
		chat.cachedContextCore(ctx, "u1", ctxCacheStubProvider{}, doc, nil, req)
		if scrubs.Load() != scrubbed+1 {
			t.Errorf("%s: the chat context was served from the cache", what)
		}
	}
	// cached asserts that each cache hits (the control: a drop is not a cache that never held anything).
	cached := func(doc *models.FlowDocument) {
		t.Helper()
		builds, scrubbed := store.builds.Load(), scrubs.Load()
		dashboard.BuildHome(ctx, "u1")
		chat.cachedContextCore(ctx, "u1", ctxCacheStubProvider{}, doc, nil, req)
		if _, ok := flows.idxCache.Get(searchIndexKey(t, id)); !ok || store.builds.Load() != builds || scrubs.Load() != scrubbed {
			t.Fatal("fixture: the caches did not hold the fill")
		}
	}

	doc := fill()
	cached(doc)
	stored, err := lib.GetLibraryFlow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stored.Name = "Renamed"
	if err := lib.UpdateLibraryFlow(ctx, stored); err != nil {
		t.Fatalf("library update: %v", err)
	}
	dropped("library update", doc)

	doc = fill()
	cached(doc)
	if _, err := flows.SaveSource(ctx, doc, eventsTestSrc+"\n", ""); err != nil {
		t.Fatalf("save source: %v", err)
	}
	dropped("flow save", doc)
}

// A home or analytics build that read before an invalidation committed is not stored: the publisher's
// transaction published (immediate pass), the build started and read the old rows, the transaction committed
// (committed pass), and only then did the build finish.
func TestDashboardFill_ReadBeforeACommittedInvalidationIsRefused(t *testing.T) {
	store := &gatedDashStore{FakeBackend: testutil.NewFakeBackend(), gate: make(chan struct{}), entered: make(chan struct{})}
	dashboard := NewDashboardService(store, nil, nil, nil)
	bus := subscribedBus(&FlowService{}, dashboard, &ChatService{})

	txCtx, reg := txhook.With(context.Background())
	bus.Publish(txCtx, events.FlowChanged{FlowID: "f1", OwnerID: "u1", Kind: events.FlowUpdated})
	done := make(chan struct{})
	go func() {
		defer close(done)
		dashboard.BuildHome(context.Background(), "u1")
	}()
	<-store.entered // the build is reading the pre-commit rows
	reg.Commit(nil)
	close(store.gate)
	<-done

	store.gate = nil
	dashboard.BuildHome(context.Background(), "u1")
	if got := store.builds.Load(); got != 2 {
		t.Fatalf("home builds = %d, want 2: the stale build was cached", got)
	}
	dashboard.BuildHome(context.Background(), "u1")
	if got := store.builds.Load(); got != 2 {
		t.Fatalf("home builds = %d, want 2: a clean build must still fill", got)
	}
}

// gatedScope blocks one scope read until released, so an invalidation can land in the middle of a build.
type gatedScope struct {
	stubScope
	gate    chan struct{}
	entered chan struct{}
}

func (s *gatedScope) ScopeFlowIDs(ctx context.Context, userID, orgID string, scope LibraryScope, limit int) ([]string, []string, error) {
	if s.gate != nil {
		s.entered <- struct{}{}
		<-s.gate
	}
	return s.stubScope.ScopeFlowIDs(ctx, userID, orgID, scope, limit)
}

// An org flow's change drops every analytics view spanning the organisation (another member's included) and
// refuses a member's build that was reading while it committed; a view of another organisation survives.
func TestAnalyticsFill_OrgFlowChangeDropsAndFencesMemberViews(t *testing.T) {
	q := anaQuery(30)
	scope := &gatedScope{stubScope: stubScope{ids: []string{"A"}, orgIDs: []string{"org-1"}}}
	dashboard := NewDashboardService(newAnaStub(), nil, nil, nil)
	dashboard.scope = scope
	bus := subscribedBus(&FlowService{}, dashboard, &ChatService{})
	build := func(user string) {
		t.Helper()
		if _, err := dashboard.BuildAnalytics(context.Background(), user, AnalyticsCaller{}, q); err != nil {
			t.Fatalf("BuildAnalytics: %v", err)
		}
	}

	build("member")
	scope.orgIDs = []string{"org-2"}
	build("outsider")
	if scope.count() != 2 {
		t.Fatalf("fixture: %d scope reads", scope.count())
	}
	bus.Publish(context.Background(), events.FlowChanged{FlowID: "f1", OwnerID: "owner", OrgID: "org-1", Kind: events.FlowUpdated})
	scope.orgIDs = []string{"org-1"}
	build("member")
	scope.orgIDs = []string{"org-2"}
	build("outsider")
	if scope.count() != 3 {
		t.Fatalf("scope reads = %d, want 3: the member's org view is rebuilt, the outsider's is served", scope.count())
	}

	// A build reading while another org-1 flow changes and commits is not stored.
	scope.orgIDs = []string{"org-1"}
	scope.gate, scope.entered = make(chan struct{}), make(chan struct{})
	dashboard.InvalidateDashboardCache("member")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		build("member")
	}()
	<-scope.entered
	txCtx, reg := txhook.With(context.Background())
	bus.Publish(txCtx, events.FlowChanged{FlowID: "f2", OwnerID: "owner", OrgID: "org-1", Kind: events.FlowCreated})
	reg.Commit(nil)
	close(scope.gate)
	wg.Wait()
	scope.gate = nil
	before := scope.count()
	build("member")
	if scope.count() != before+1 {
		t.Fatal("a build that read while the organisation's flow changed was cached")
	}
	build("member")
	if scope.count() != before+1 {
		t.Fatal("a clean build must still fill")
	}
}

// A request whose own transaction has published a change fills no cache: what it read may be its uncommitted
// writes. Once the transaction ends, fills resume.
func TestFlowCaches_ARequestThatPublishedDoesNotFill(t *testing.T) {
	store := &gatedDashStore{FakeBackend: testutil.NewFakeBackend()}
	dashboard := NewDashboardService(store, nil, nil, nil)
	scope := &stubScope{ids: []string{"A"}}
	dashboard.scope = scope
	scrubs := countScrubs(t)
	chat := &ChatService{chatCtxCache: newChatContextCache()}
	bus := subscribedBus(&FlowService{}, dashboard, chat)
	doc := toolLoopDoc()
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}
	q := anaQuery(30)

	txCtx, reg := txhook.With(context.Background())
	bus.Publish(txCtx, events.FlowChanged{FlowID: doc.ID, OwnerID: "u1", Kind: events.FlowUpdated})
	for range 2 {
		dashboard.BuildHome(txCtx, "u1")
		if _, err := dashboard.BuildAnalytics(txCtx, "u1", AnalyticsCaller{}, q); err != nil {
			t.Fatal(err)
		}
		chat.cachedContextCore(txCtx, "u1", ctxCacheStubProvider{}, doc, nil, req)
	}
	if store.builds.Load() != 2 || scope.count() != 2 || scrubs.Load() != 2 {
		t.Fatalf("builds home=%d analytics=%d chat=%d inside the publishing transaction, want 2 each (no fill)",
			store.builds.Load(), scope.count(), scrubs.Load())
	}

	reg.Commit(nil)
	for range 2 {
		dashboard.BuildHome(txCtx, "u1")
		if _, err := dashboard.BuildAnalytics(txCtx, "u1", AnalyticsCaller{}, q); err != nil {
			t.Fatal(err)
		}
		chat.cachedContextCore(txCtx, "u1", ctxCacheStubProvider{}, doc, nil, req)
	}
	if store.builds.Load() != 3 || scope.count() != 3 || scrubs.Load() != 3 {
		t.Fatalf("builds home=%d analytics=%d chat=%d after the commit, want 3 each (one fill, then hits)",
			store.builds.Load(), scope.count(), scrubs.Load())
	}
}

// A chat context built from a document read before an edit committed is not served to a turn holding the
// edited document, even when the build captured the generation after the edit's invalidation; and a build an
// invalidation overlapped stores nothing.
func TestChatContextFill_StaleDocumentAndOverlappedBuildAreNotServed(t *testing.T) {
	scrubs := countScrubs(t)
	chat := &ChatService{chatCtxCache: newChatContextCache()}
	req := models.ChatRequest{Model: "m1", UserMessage: "q"}
	before := toolLoopDoc()

	// The turn resolved `before`; the edit commits and invalidates; then the turn builds and caches.
	chat.InvalidateChatContext(before.ID)
	stale := chat.cachedContextCore(context.Background(), "s", ctxCacheStubProvider{}, before, nil, req)
	edited := toolLoopDoc()
	edited.Subflows[0].Blocks[0].Name = "Edited"
	edited.RebuildIndexes()
	fresh := chat.cachedContextCore(context.Background(), "s", ctxCacheStubProvider{}, edited, nil, req)
	if fresh.scrubbedDoc == stale.scrubbedDoc || scrubs.Load() != 2 {
		t.Fatal("the turn holding the edited flow was served the context built from the pre-edit document")
	}

	// An invalidation while the build runs: nothing is stored under the generation the build captured.
	orig := scrubDocument
	scrubDocument = func(doc *models.FlowDocument) (*models.FlowDocument, error) {
		chat.InvalidateChatContext(doc.ID)
		return orig(doc)
	}
	other := toolLoopDoc()
	other.ID = "f-overlapped"
	gen := chat.chatCtxGen.Load(other.ID)
	chat.cachedContextCore(context.Background(), "s", ctxCacheStubProvider{}, other, nil, req)
	scrubDocument = orig
	cc := chat.chatCtxCache
	key := chat.chatContextKeyAt("s", other.ID, gen, req, ctxCacheStubProvider{}.ID(), req.Model, nil, "", defaultContextTokenBudget)
	if _, ok := cc.texts.Get(key); ok {
		t.Error("an overlapped build stored its texts")
	}
	if _, ok := cc.docs.Get(scrubbedDocKey("s", other.ID, gen)); ok {
		t.Error("an overlapped build stored its clone")
	}
}

// The FlowService announces each cloud write once, with the stored version, and the desktop single-file batch
// fix (which invalidated nothing before) announces too.
func TestFlowService_AnnouncesCloudWritesWithTheStoredVersion(t *testing.T) {
	backend := testutil.NewFakeBackend()
	bus := eventbus.New(nil, time.Second)
	rec := eventbus.NewRecorder(bus)
	flows := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, NewAuthzService(backend, nil), nil, WithFlowEvents(bus))
	ctx := actorCtx("u1")

	up, err := parser.ParseText(eventsTestSrc, "Main.txt", int64(len(eventsTestSrc)))
	if err != nil {
		t.Fatal(err)
	}
	up.Source = eventsTestSrc
	if err := flows.SaveUploadedFlow(ctx, up, "u1"); err != nil {
		t.Fatal(err)
	}
	backend.Flows[up.ID].OrganizationID = "org-1"
	doc, err := flows.DocProvider().ResolveDoc(ctx, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flows.SaveSource(ctx, doc, eventsTestSrc+"\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := flows.SaveUploadedFlow(ctx, up, "u1"); err != nil {
		t.Fatal(err)
	}
	got := eventbus.Recorded[events.FlowChanged](rec)
	want := []events.FlowChanged{
		{FlowID: up.ID, StableID: up.ID, OwnerID: "u1", Kind: events.FlowCreated, Version: 0},
		{FlowID: up.ID, StableID: up.ID, OwnerID: "u1", OrgID: "org-1", Kind: events.FlowUpdated, Version: 1},
		{FlowID: up.ID, StableID: up.ID, OwnerID: "u1", OrgID: "org-1", Kind: events.FlowUpdated, Version: 2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("announced\n%+v\nwant\n%+v", got, want)
	}

	desktop, ddoc := newDesktopEditSvc(t)
	drec := eventbus.NewRecorder(bus)
	desktop.bus = bus
	res, err := desktop.ApplyFixBatchResult(context.Background(), ddoc, nil, 0)
	if err != nil || res.Applied == 0 {
		t.Fatalf("fixture: batch fix applied %d: %v", res.Applied, err)
	}
	if got := eventbus.Recorded[events.FlowChanged](drec); len(got) != 1 || got[0].FlowID != ddoc.ID || got[0].Kind != events.FlowUpdated || got[0].StableID == "" {
		t.Errorf("desktop batch fix announced %+v, want one update of %s", got, ddoc.ID)
	}
}
