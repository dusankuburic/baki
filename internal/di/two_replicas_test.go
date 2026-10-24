package di

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/invalidation"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/scanner"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/cache"
	"pad-core/models"
)

// Two replicas of the app on one database and one Redis, assembled from the real components with the providers the
// fx graph uses (docs/architecture/operations.md, "Running more than one replica"): each replica has its own event
// bus with the production subscribers and the cache invalidation fan-out, its own event manager with the SSE fan-out,
// its own flow, chat and settings services on its own Redis client. The scenarios run across the two as
// the mechanisms' own tests cannot: one flow of work touches the database and Redis and a second replica together,
// and Redis is killed and restored in the middle of it. The mechanisms' unit tests (internal/redisx, internal/api,
// internal/service, internal/invalidation) cover each part alone; this proves they work side by side on one
// deployment's data.
//
// The scenarios share one stack and run in order: each leaves behind what the next one needs (a flow, a user, a
// running stream). They run on the in-memory backend always, and on a Postgres database of their own when
// DATABASE_URL is set (two connection pools on one database, as two replicas have).

const twoReplicaUser = "u-two-replicas"

// xrSrc is a one-block flow; xrFixSrc a flow with a finding the chat's apply_fix can propose a fix for.
const xrSrc = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://x/%s''' Method: HTTPClient.Method.GET\n#EndRegion\n"

// xrProviders registers the stub models the scenarios stream with, once per test binary (ai.RegisterProvider is
// process-global). xr-blocking streams nothing until its stream is stopped. A model that asks for a fix and then answers is registered
// per stream (startStreamWith): the fix names a block of the flow under test.
var xrProviders sync.Once

type xrBlocking struct{ *testutil.FakeProvider }

func (p *xrBlocking) Stream(ctx context.Context, _ ai.Request, _ func(ai.Chunk)) error {
	<-ctx.Done()
	return ctx.Err()
}

func registerXRProviders() {
	xrProviders.Do(func() {
		ai.RegisterProvider("xr-blocking", func(string) ai.Provider { return &xrBlocking{FakeProvider: &testutil.FakeProvider{}} })
	})
}

// xrReplica is one replica of the stack.
type xrReplica struct {
	id       string
	rep      *redisreplica.Replica
	backend  storageif.StorageBackend
	bus      *eventbus.Bus
	settings *service.BackendSettingsProvider
	profiles *service.RuleProfileResolver
	dash     *service.DashboardService
	docs     *service.CloudDocumentProvider
	flows    *service.FlowService
	events   *api.EventManager
	srv      *httptest.Server
	chat     *service.ChatService
	factory  *ai.ProviderFactory
	analysis *service.AnalysisService
	// scan is the replica's governance sweep; scanRuns counts the flows it analyzed.
	scan     *scanner.Scanner
	scanRuns atomic.Int64
}

// newXRReplica assembles a replica on rep's Redis client and the given store: the same constructors, in the same
// shapes, as the providers of ServiceModule and APIModule.
func newXRReplica(t *testing.T, rep *redisreplica.Replica, backend storageif.StorageBackend, cfg *config.Config) *xrReplica {
	t.Helper()
	ctx := context.Background()
	lc := fxtest.NewLifecycle(t)
	r := &xrReplica{id: rep.ID, rep: rep, backend: backend, bus: eventbus.New(nil, time.Second)}

	r.settings = service.NewBackendSettingsProvider(ctx, backend)
	r.profiles = service.NewRuleProfileResolver(backend, r.settings, nil)
	r.docs = service.NewCloudDocumentProvider(backend)
	r.events = ProvideEventManager(lc, cfg, make(chan struct{}), backend, rep.Client, rep.Health)
	r.events.SetClientKeyFunc(func(req *http.Request) string { return "user:" + req.Header.Get("X-Test-User") })
	r.srv = httptest.NewServer(http.HandlerFunc(r.events.HandleEvents))
	t.Cleanup(r.srv.Close)

	analyzer := ProvideAnalyzer()
	// The flow service decides flow permissions as in production; the chat gate asks it whether the caller may edit.
	r.flows = ProvideFlowService(cfg, r.events, r.settings, r.docs, backend, service.NewAuthzService(backend, nil), ProvideASTCache(), r.bus, analyzer, rep.Client)
	analysis, err := service.NewAnalysisService(r.events, r.settings, nil, service.AnalysisOptions{Analyzer: analyzer, Bus: r.bus, Shared: replicatedClient(rep.Client, cfg)})
	if err != nil {
		t.Fatal(err)
	}
	r.analysis = analysis
	// The sweep as internal/app builds it with PAD_MAX_REPLICAS > 1 (provideScanner), without a schedule.
	r.scan = scanner.New(backend, nil, func(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		r.scanRuns.Add(1)
		return analysis.AnalyzeFlowBackground(ctx, doc)
	}, nil, 0, nil, scanner.WithSharedScans(replicatedClient(rep.Client, cfg)))
	r.scan.SetRuleSetDigest(func(ctx context.Context, orgID string) string {
		p := r.profiles.Resolve(ctx, orgID)
		return scanner.DigestRuleSet(p.Settings, p.Rules)
	})
	t.Cleanup(r.scan.Stop)
	r.dash = service.NewDashboardService(backend, analysis, r.flows, nil)
	r.factory = ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	r.chat = service.NewChatService(r.events, t.TempDir(), r.flows, analysis, r.settings, r.factory, backend, config.ModeCloud, service.ChatOptions{
		Resume: rep.Client, Instance: rep.ID,
		MultiReplica: multiReplicaClient(rep.Client, cfg) != nil, Health: rep.Health,
	})
	r.chat.StartCommands()
	t.Cleanup(func() { _ = r.chat.Shutdown(context.Background()) })

	// The model listings and the knowledge base's corpus counts are replicated caches too: a resync reaches their
	// appliers, so they exist (nothing here lists models or reads a corpus).
	providers := service.NewProviderService(nil, nil, r.factory, nil, nil, nil, service.WithProviderEvents(r.bus))
	knowledge := ProvideKnowledgeService(backend, r.factory, r.settings, cfg, nil, r.bus, nil)
	SubscribeFlowCaches(r.bus, r.flows, r.dash, r.chat)
	SubscribeSettingsAndCredentialCaches(r.bus, r.settings, r.profiles, providers, knowledge)
	stop := invalidation.Install(rep.Client, rep.Health, r.bus, rep.ID,
		invalidation.WithHeartbeat(time.Hour), invalidation.WithRetry(20*time.Millisecond, 100*time.Millisecond), invalidation.WithResyncGap(100*time.Millisecond))
	lc.RequireStart()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		stop(sctx)
		lc.RequireStop()
	})
	return r
}

// xrConfig is the configuration of a cloud deployment with shared state, as the capability checks read it.
func xrConfig(rep *redisreplica.Replica, addr string) *config.Config {
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	cfg.Auth.Enabled = true
	cfg.Storage.Backend = config.StorageDatabase
	cfg.Storage.MaxReplicas = 2
	cfg.Redis.URL = secret.New("redis://" + addr)
	cfg.Runtime.InstanceID = rep.ID
	return cfg
}

// xrStack is the two replicas and what they share.
type xrStack struct {
	cluster *redisreplica.Cluster
	a, b    *xrReplica
	flowID  string
	doc     *models.FlowDocument
}

func newXRStack(t *testing.T, backendFor func(i int) storageif.StorageBackend) *xrStack {
	t.Helper()
	registerXRProviders()
	// The deployment keeps conversation history, as a configured one does (the default drops saves).
	seed := models.DefaultSettings()
	seed.AI.SaveConversationHistory = true
	if err := backendFor(0).SaveSettings(context.Background(), settingsToStorage(t, seed)); err != nil {
		t.Fatalf("seed the settings: %v", err)
	}
	c := redisreplica.New(t, 2)
	s := &xrStack{cluster: c}
	resyncs := resyncCount("subscribe")
	s.a = newXRReplica(t, c.Replicas[0], backendFor(0), xrConfig(c.Replicas[0], c.Redis.Addr()))
	s.b = newXRReplica(t, c.Replicas[1], backendFor(1), xrConfig(c.Replicas[1], c.Redis.Addr()))
	// Both replicas must be listening before a scenario emits anything.
	for _, ch := range []string{"baki:inval", "baki:sse:ev"} {
		redisreplica.Eventually(t, 5*time.Second, ch+" subscribed", func() bool { return c.Redis.PubSubNumSub(ch)[ch] >= 2 })
	}
	// A command channel carries the replica's boot suffix (baki:chat:cmd:replica-1#3fa91c0e), so match its prefix.
	for _, id := range []string{"replica-1", "replica-2"} {
		pattern := "baki:chat:cmd:" + id + "#*"
		redisreplica.Eventually(t, 5*time.Second, pattern+" subscribed", func() bool {
			for _, ch := range c.Redis.PubSubChannels(pattern) {
				if c.Redis.PubSubNumSub(ch)[ch] >= 1 {
					return true
				}
			}
			return false
		})
	}
	// The resync each subscription starts with drops the caches: let it finish before a scenario fills any.
	// (The count is read best effort: replicas whose resyncs coalesce still settle within the pause.)
	for deadline := time.Now().Add(2 * time.Second); resyncCount("subscribe")-resyncs < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return s
}

// xrBackends are the stores the scenarios run on.
func xrBackends(t *testing.T) map[string]func(*testing.T) func(int) storageif.StorageBackend {
	return map[string]func(*testing.T) func(int) storageif.StorageBackend{
		"memory store": func(*testing.T) func(int) storageif.StorageBackend {
			fake := testutil.NewFakeBackend()
			return func(int) storageif.StorageBackend { return fake }
		},
		"postgres": func(t *testing.T) func(int) storageif.StorageBackend {
			dsn := pgtest.NewDatabase(t, "")
			var pools []storageif.StorageBackend
			for i := 0; i < 2; i++ {
				pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(dsn))
				if err != nil {
					t.Fatalf("open the test database: %v", err)
				}
				t.Cleanup(func() { pg.Close() })
				pools = append(pools, pg)
			}
			return func(i int) storageif.StorageBackend { return pools[i] }
		},
	}
}

func TestTwoReplicas_EndToEnd(t *testing.T) {
	for name, open := range xrBackends(t) {
		t.Run(name, func(t *testing.T) {
			s := newXRStack(t, open(t))
			if err := s.a.backend.SaveUser(context.Background(), &storageif.User{ID: twoReplicaUser, Email: "two@example.com", Password: "hash", Role: auth.RoleMember}); err != nil {
				t.Fatalf("seed the user: %v", err)
			}
			for _, step := range []struct {
				name string
				run  func(*testing.T, *xrStack)
			}{
				{"a flow saved on A shows in B's home view at once", scenarioFlowFanOut},
				{"an event emitted on A reaches B's stream and replays after a reconnect", scenarioEventsAndReplay},
				{"concurrent saves of one conversation, one per replica, have one winner", scenarioConversationSaves},
				{"an edit on A is undone through B", scenarioUndoOnTheOtherReplica},
				{"a user's event streams, chat streams and stream ids count on both replicas", scenarioCapsSpanTheReplicas},
				{"a deployment setting changed on A reaches B before its cache expires", scenarioSettingsReachThePeer},
				{"a stop and an approval decision on B reach the stream running on A", scenarioStreamCommands},
				{"a flow swept or analyzed on A is not analyzed or recorded again on B, which exports and diffs A's runs", scenarioAnalysesAcrossReplicas},
				{"Redis killed and restored mid-run: degraded, then healed", scenarioRedisOutage},
			} {
				t.Run(step.name, func(t *testing.T) { step.run(t, s) })
			}
		})
	}
}

// --- helpers shared by the scenarios ---------------------------------------------------------------------------------

type sseFrame struct{ ID, Data string }

// openStream connects to rep's event stream as user, resuming after lastID when given. Frames arrive on the channel
// until the returned func closes the stream.
func openStream(t *testing.T, r *xrReplica, user, lastID string) (<-chan sseFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", user)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	frames := make(chan sseFrame, 256)
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); _ = resp.Body.Close() }) }
	t.Cleanup(closeStream)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur sseFrame
		have := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if have {
					frames <- cur
				}
				cur, have = sseFrame{}, false
			case strings.HasPrefix(line, "id: "):
				cur.ID, have = strings.TrimPrefix(line, "id: "), true
			case strings.HasPrefix(line, "data: "):
				cur.Data, have = strings.TrimPrefix(line, "data: "), true
			}
		}
	}()
	return frames, closeStream
}

// nextFrame waits for a frame matching want ("" = any).
func nextFrame(t *testing.T, ch <-chan sseFrame, what string, want func(sseFrame) bool) sseFrame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", what)
			}
			if want == nil || want(f) {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func eventNamed(name, contains string) func(sseFrame) bool {
	return func(f sseFrame) bool {
		var ev struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(f.Data), &ev)
		return ev.Name == name && strings.Contains(f.Data, contains)
	}
}

// uploadFlow stores a one-block flow for the user through r, as the upload handler does.
func uploadFlow(t *testing.T, r *xrReplica, name string) *models.FlowDocument {
	t.Helper()
	ctx := context.Background()
	src := strings.Replace(xrSrc, "%s", name, 1)
	doc, err := r.flows.LoadFlowFiles(ctx, map[string]string{"Main.txt": src}, name, twoReplicaUser)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	doc.Source = src
	if err := r.flows.SaveUploadedFlow(ctx, doc, twoReplicaUser); err != nil {
		t.Fatalf("save %s: %v", name, err)
	}
	return doc
}

// homeMisses builds r's home view of the user and reports how many times the process has had to build one: a build
// that is served from the cache does not move it, a rebuilt one does.
func homeMisses(t *testing.T, r *xrReplica) uint64 {
	t.Helper()
	r.dash.BuildHome(context.Background(), twoReplicaUser)
	for _, spec := range cache.Specs() {
		if spec.Name == "dashboard.home" {
			return spec.Stats.Misses
		}
	}
	t.Fatal("dashboard.home is not declared")
	return 0
}

// --- scenarios -------------------------------------------------------------------------------------------------------

// An upload on A drops B's cached home view of the owner: B shows the new flow when the message lands, not 30 s later.
func scenarioFlowFanOut(t *testing.T, s *xrStack) {
	before := homeMisses(t, s.b)
	if homeMisses(t, s.b) != before {
		t.Fatal("premise: B serves the same view twice from its cache")
	}
	s.doc = uploadFlow(t, s.a, "first")
	s.flowID = s.doc.ID
	// B's cached view of the owner is dropped by the message, so its next load builds a new one: well inside the
	// 30 s the cache would otherwise serve the old view. (The in-memory store computes no flow counts, so the
	// rebuild is what is observed.)
	redisreplica.Eventually(t, 3*time.Second, "B to rebuild the owner's home view", func() bool { return homeMisses(t, s.b) > before })
}

// A client of B holds a stream; an event emitted on A reaches it with an id; the client reconnects with that id to
// A and is replayed what it missed meanwhile, and nothing it had seen.
func scenarioEventsAndReplay(t *testing.T, s *xrStack) {
	user := twoReplicaUser
	frames, closeB := openStream(t, s.b, user, "")
	redisreplica.Eventually(t, 3*time.Second, "B to record the user's stream in Redis", func() bool {
		// The subscriber field is the replica's origin, its id plus the boot suffix.
		keys, _ := s.cluster.Redis.HKeys(redisx.SSESubsKey.Key(user))
		for _, k := range keys {
			if redisx.OriginInstance(k) == s.b.id {
				return true
			}
		}
		return false
	})

	s.a.events.EmitTo(user, "flow:loaded", map[string]string{"name": "seen-1"})
	f1 := nextFrame(t, frames, "the first event on B", eventNamed("flow:loaded", "seen-1"))
	if f1.ID == "" {
		t.Fatal("the event crossed to B without an id: it cannot be replayed")
	}
	closeB()

	s.a.events.EmitTo(user, "flow:loaded", map[string]string{"name": "missed-2"})
	s.a.events.EmitTo(user, "flow:loaded", map[string]string{"name": "missed-3"})

	replayed, closeA := openStream(t, s.a, user, f1.ID)
	defer closeA()
	nextFrame(t, replayed, "the first missed event", eventNamed("flow:loaded", "missed-2"))
	nextFrame(t, replayed, "the second missed event", eventNamed("flow:loaded", "missed-3"))
	select {
	case f := <-replayed:
		if strings.Contains(f.Data, "seen-1") {
			t.Fatalf("the replay repeated an event the client had seen: %+v", f)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

// Two saves of one conversation at the same version, one through each replica: the database lets exactly one in, the
// other is told to reload. Repeated with the replicas swapped.
func scenarioConversationSaves(t *testing.T, s *xrStack) {
	ctx := context.Background()
	key := "xr-conversation"
	for round := 0; round < 6; round++ {
		first, second := s.a, s.b
		if round%2 == 1 {
			first, second = s.b, s.a
		}
		read, err := first.chat.ReadConversation(ctx, twoReplicaUser, s.flowID, key)
		if err != nil || !read.Versioned {
			t.Fatalf("round %d: read = %+v, %v", round, read, err)
		}
		base := read.Version
		var wg sync.WaitGroup
		results := make([]error, 2)
		saved := make([]service.ConversationSaved, 2)
		for i, r := range []*xrReplica{first, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				msgs := []models.ChatMessage{{ID: fmt.Sprintf("m-%d-%d", round, i), Role: "user", Content: "from " + r.id}}
				saved[i], results[i] = r.chat.SaveClientConversation(ctx, s.flowID, key, &base, msgs)
			}()
		}
		wg.Wait()
		wins, conflicts := 0, 0
		var winner service.ConversationSaved
		for i, err := range results {
			switch {
			case err == nil:
				wins++
				winner = saved[i]
			case errors.Is(err, storageif.ErrConversationConflict):
				conflicts++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d winners and %d conflicts at version %d, want one of each", round, wins, conflicts, base)
		}
		// The loser reloads and sees the winner's turn, on either replica.
		after, err := second.chat.ReadConversation(ctx, twoReplicaUser, s.flowID, key)
		if err != nil || after.Version <= base || after.Version != winner.Version {
			t.Fatalf("round %d: version after = %d (%v); want the winner's %d, past %d", round, after.Version, err, winner.Version, base)
		}
	}
}

// A source edit on A leaves an undo snapshot in the database; B lists it and restores it, and A then serves the
// original source.
func scenarioUndoOnTheOtherReplica(t *testing.T, s *xrStack) {
	ctx := context.Background()
	docA, err := s.a.docs.ResolveDoc(ctx, s.flowID)
	if err != nil {
		t.Fatalf("resolve on A: %v", err)
	}
	original := docA.Source
	if _, err := s.a.flows.SaveSource(ctx, docA, original+"\n\n", ""); err != nil {
		t.Fatalf("SaveSource on A: %v", err)
	}
	docB, err := s.b.docs.ResolveDoc(ctx, s.flowID)
	if err != nil {
		t.Fatalf("resolve on B: %v", err)
	}
	snaps, err := s.b.flows.ListSourceSnapshots(ctx, docB)
	if err != nil || len(snaps) == 0 {
		t.Fatalf("B lists %d undo snapshots (%v) of an edit made on A", len(snaps), err)
	}
	if _, err := s.b.flows.RestoreSourceSnapshot(ctx, docB, snaps[len(snaps)-1].ID); err != nil {
		t.Fatalf("restore on B: %v", err)
	}
	s.a.docs.Invalidate(s.flowID)
	back, err := s.a.docs.ResolveDoc(ctx, s.flowID)
	if err != nil || back.Source != original {
		t.Fatalf("A serves %q after the undo on B (%v), want the original %q", back.Source, err, original)
	}
}

// An admin changes the deployment's embedding model on A: B reads it as soon as the message lands.
func scenarioSettingsReachThePeer(t *testing.T, s *xrStack) {
	ctx := context.Background()
	if got := s.b.settings.Get().AI.EmbeddingModel; got == "xr-embed-1" {
		t.Fatalf("premise: B already reads the model the scenario sets, %q", got)
	}
	next := *s.a.settings.Get()
	next.AI.EmbeddingModel = "xr-embed-1"
	// The write is announced after it commits, as the settings service does (the memory store has no transaction).
	if err := s.a.settings.Update(ctx, next); err != nil {
		t.Fatal(err)
	}
	s.a.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	redisreplica.Eventually(t, 3*time.Second, "B to read the new embedding model", func() bool { return s.b.settings.Get().AI.EmbeddingModel == "xr-embed-1" })
}

// The stream runs on A; the stop and the approval decision arrive at B.
func scenarioStreamCommands(t *testing.T, s *xrStack) {
	ctx := context.Background()
	user := twoReplicaUser

	// A stop. Only the stream's owner may give it: another user's stop, through either replica, changes nothing.
	stopID := startStream(t, s.a, s.doc, "xr-blocking")
	redisreplica.Eventually(t, 3*time.Second, "the stream's snapshot in Redis", func() bool {
		r, err := s.b.chat.ResumeStream(ctx, stopID, 0)
		return err == nil && r != nil
	})
	if err := s.b.chat.CancelStreamAs(ctx, "someone-else", stopID); !errors.Is(err, service.ErrStreamNotOwned) {
		t.Errorf("a stop by another user through B = %v, want ErrStreamNotOwned", err)
	}
	if r, _ := s.a.chat.ResumeStream(ctx, stopID, 0); r == nil || r.Done || r.ErrorCode != "" {
		t.Fatalf("the stream was affected by a stop it does not own: %+v", r)
	}
	if err := s.b.chat.CancelStreamAs(ctx, user, stopID); err != nil {
		t.Fatalf("stop through B: %v", err)
	}
	redisreplica.Eventually(t, 3*time.Second, "the stream on A to end as stopped", func() bool {
		r, err := s.a.chat.ResumeStream(ctx, stopID, 0)
		return err == nil && r.ErrorCode == service.ChatErrStopped
	})

	// An approval.
	blockID := s.doc.Subflows[0].Blocks[0].ID
	approvalID := startStreamWith(t, s.a, user, s.doc, "xr-tools", blockID)
	var proposal string
	redisreplica.Eventually(t, 5*time.Second, "the fix proposal waiting in Redis", func() bool {
		for _, k := range s.cluster.Redis.Keys() {
			if strings.HasPrefix(k, "baki:chat:approval:") {
				proposal = strings.TrimPrefix(k, "baki:chat:approval:")
				return true
			}
		}
		return false
	})
	writer := auth.WithActor(ctx, &auth.Actor{Kind: auth.ActorToken, UserID: user, Scopes: []string{auth.ScopeChat, auth.ScopeWrite}})
	if err := s.b.chat.DecideApproval(writer, user, approvalID, proposal, false, nil); err != nil {
		t.Fatalf("decision through B: %v", err)
	}
	redisreplica.Eventually(t, 5*time.Second, "the stream on A to finish after the decision", func() bool {
		r, err := s.a.chat.ResumeStream(ctx, approvalID, 0)
		return err == nil && r.Done
	})
	r, _ := s.a.chat.ResumeStream(ctx, approvalID, 0)
	if !strings.Contains(r.Text, "All done.") {
		t.Errorf("the stream ended with %q after the decision, want the model's answer", r.Text)
	}
	if err := s.b.chat.DecideApproval(writer, user, approvalID, proposal, true, nil); !errors.Is(err, service.ErrNoPendingFix) {
		t.Errorf("a second decision = %v, want ErrNoPendingFix", err)
	}
}

// The per-user caps hold across the replicas: 10 event streams on A leave none on B, 3 chat streams on A leave none on
// B, and a stream id running on A cannot start a second stream on B. Everything is given back at the end.
func scenarioCapsSpanTheReplicas(t *testing.T, s *xrStack) {
	user := "u-caps"
	status := func(r *xrReplica) int {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL, nil)
		req.Header.Set("X-Test-User", user)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
		return resp.StatusCode
	}
	for i := 0; i < 10; i++ {
		if code := status(s.a); code != http.StatusOK {
			t.Fatalf("event stream %d on A: %d", i+1, code)
		}
	}
	if code := status(s.b); code != http.StatusServiceUnavailable {
		t.Errorf("an 11th event stream on B: %d, want 503", code)
	}

	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, startStreamWith(t, s.a, user, s.doc, "xr-blocking", ""))
	}
	_, err := s.b.chat.StreamChatMessage(context.Background(), user, s.doc, nil, models.ChatRequest{
		Provider: "xr-blocking", Model: "fake-model", UserMessage: "go", ClientStreamID: uuid.NewString(),
	})
	if !errors.Is(err, service.ErrTooManyChatStreams) {
		t.Errorf("a 4th chat stream on B = %v, want ErrTooManyChatStreams", err)
	}
	_, err = s.b.chat.StreamChatMessage(context.Background(), "u-other", s.doc, nil, models.ChatRequest{
		Provider: "xr-blocking", Model: "fake-model", UserMessage: "go", ClientStreamID: ids[0],
	})
	if err == nil || !strings.Contains(err.Error(), "clientStreamId already in use") {
		t.Errorf("a stream on B under an id running on A = %v, want the id refused", err)
	}
	for _, id := range ids {
		s.a.chat.CancelStream(id)
	}
	redisreplica.Eventually(t, 5*time.Second, "a chat stream slot free on B after A's streams ended", func() bool {
		id := uuid.NewString()
		_, err := s.b.chat.StreamChatMessage(context.Background(), user, s.doc, nil, models.ChatRequest{
			Provider: "xr-blocking", Model: "fake-model", UserMessage: "go", ClientStreamID: id,
		})
		if err == nil {
			s.b.chat.CancelStream(id)
		}
		return err == nil
	})
}

func startStream(t *testing.T, r *xrReplica, doc *models.FlowDocument, provider string) string {
	return startStreamWith(t, r, twoReplicaUser, doc, provider, "")
}

// startStreamWith starts a chat turn of user on r with the stub provider; a blockID makes the model ask for a fix
// of that block (and the turn use tools).
func startStreamWith(t *testing.T, r *xrReplica, user string, doc *models.FlowDocument, provider, blockID string) string {
	t.Helper()
	if blockID != "" {
		ai.RegisterProvider(provider, func(string) ai.Provider {
			return &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
				{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"` + blockID + `","ruleId":"unhandled-error"}`)}}},
				{Text: "All done."},
			}}
		})
	}
	id := uuid.NewString()
	// A cloud stream always carries its caller: the gate offers apply_fix only to one who may edit the flow (ADR 0038).
	ctx := auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorUser, UserID: user, Role: auth.RoleMember})
	if _, err := r.chat.StreamChatMessage(ctx, user, doc, nil, models.ChatRequest{
		Provider: provider, Model: "fake-model", UserMessage: "go", ClientStreamID: id, UseTools: blockID != "",
	}); err != nil {
		t.Fatalf("StreamChatMessage on %s: %v", r.id, err)
	}
	t.Cleanup(func() { r.chat.CancelStream(id) })
	return id
}

// The governance sweep and an interactive analysis on A; B's sweep after A's analyzes nothing, the same run analyzed on
// B is not recorded a second time, and after an edit analyzed on A, B exports A's run and diffs against the one before
// (with the pair per replica, B answered "run analysis first" and had no previous run).
func scenarioAnalysesAcrossReplicas(t *testing.T, s *xrStack) {
	ctx := context.Background()
	s.a.scan.ScanOnce(ctx)
	if s.a.scanRuns.Load() == 0 {
		t.Fatal("premise: A's sweep analyzed no flow")
	}
	before := s.b.scanRuns.Load()
	s.b.scan.ScanOnce(ctx)
	if n := s.b.scanRuns.Load() - before; n != 0 {
		t.Errorf("B's sweep after A's analyzed %d unchanged flows again, want 0", n)
	}

	resolve := func(r *xrReplica) *models.FlowDocument {
		t.Helper()
		r.docs.Invalidate(s.flowID)
		doc, err := r.docs.ResolveDoc(ctx, s.flowID)
		if err != nil {
			t.Fatalf("resolve on %s: %v", r.id, err)
		}
		return doc
	}
	r1, fresh, err := s.a.analysis.AnalyzeFlowFresh(ctx, resolve(s.a))
	if err != nil || !fresh {
		t.Fatalf("the first interactive analysis on A = fresh %v, %v; want a fresh run", fresh, err)
	}
	if _, fresh, err := s.b.analysis.AnalyzeFlowFresh(ctx, resolve(s.b)); err != nil || fresh {
		t.Errorf("the same run analyzed on B = fresh %v, %v; want it recorded once, on A", fresh, err)
	}

	docA := resolve(s.a)
	if _, err := s.a.flows.SaveSource(ctx, docA, strings.Replace(docA.Source, "https://x/", "https://edited.example/", 1), ""); err != nil {
		t.Fatalf("SaveSource on A: %v", err)
	}
	r2, err := s.a.analysis.AnalyzeFlow(ctx, resolve(s.a))
	if err != nil || r2.ContentHash == r1.ContentHash {
		t.Fatalf("the analysis of the edit on A = %v (content unchanged: %v)", err, err == nil && r2.ContentHash == r1.ContentHash)
	}
	docB := resolve(s.b)
	if cur, ok := s.b.analysis.CurrentReport(docB); !ok || cur.ContentHash != r2.ContentHash {
		t.Errorf("B's current report of the flow (the export's) is not A's latest run (ok %v)", ok)
	}
	if _, err := s.b.analysis.AnalyzeFlow(ctx, docB); err != nil {
		t.Fatal(err)
	}
	if prev, has := s.b.analysis.PreviousReport(docB); !has || prev.ContentHash != r1.ContentHash {
		t.Errorf("B's diff has no previous run, or not A's run before the edit (has %v)", has)
	}
}

// Redis dies in the middle of it all and comes back with its data.
func scenarioRedisOutage(t *testing.T, s *xrStack) {
	ctx := context.Background()
	user := twoReplicaUser
	frames, closeStream := openStream(t, s.a, user, "")
	defer closeStream()
	blocked := startStream(t, s.a, s.doc, "xr-blocking")
	redisreplica.Eventually(t, 3*time.Second, "the stream's snapshot in Redis", func() bool {
		r, err := s.b.chat.ResumeStream(ctx, blocked, 0)
		return err == nil && r != nil
	})

	s.cluster.Kill()
	for _, r := range []*xrReplica{s.a, s.b} {
		_ = r.rep.Health.Ping(ctx)
		if !r.rep.Health.Degraded() {
			t.Fatalf("%s did not notice that Redis is gone", r.id)
		}
	}

	// Local work goes on: the owner's own stream still stops, events reach the clients of the emitting replica,
	// conversation saves are the database's, uploads work.
	s.a.events.EmitTo(user, "flow:loaded", map[string]string{"name": "during-outage"})
	nextFrame(t, frames, "a local event with Redis down", eventNamed("flow:loaded", "during-outage"))
	read, err := s.b.chat.ReadConversation(ctx, user, s.flowID, "xr-outage")
	if err != nil {
		t.Fatalf("read with Redis down: %v", err)
	}
	if _, err := s.b.chat.SaveClientConversation(ctx, s.flowID, "xr-outage", &read.Version, []models.ChatMessage{{ID: "o1", Role: "user", Content: "offline"}}); err != nil {
		t.Errorf("a conversation save with Redis down: %v", err)
	}
	uploadFlow(t, s.a, "during-outage")

	// What needs the other replica says so instead of pretending: the stop is a retryable 503, never a false ok.
	err = s.b.chat.CancelStreamAs(ctx, user, blocked)
	if !errors.Is(err, service.ErrReplicaUnreachable) {
		t.Errorf("a stop for another replica's stream with Redis down = %v, want ErrReplicaUnreachable", err)
	}
	// A cache invalidation is not delivered: B keeps its old value until the channel is back.
	changed := *s.a.settings.Get()
	changed.AI.EmbeddingModel = "xr-embed-2"
	if err := s.a.settings.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	s.a.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	time.Sleep(150 * time.Millisecond)
	if got := s.b.settings.Get().AI.EmbeddingModel; got != "xr-embed-1" {
		t.Errorf("B reads embedding model %q with the channel down, want its cached xr-embed-1", got)
	}

	// Back: both replicas heal, the resync brings B's caches up to date, commands cross again.
	s.cluster.Restore(t)
	redisreplica.Eventually(t, 10*time.Second, "both replicas to report Redis healthy", func() bool {
		ok := true
		for _, r := range []*xrReplica{s.a, s.b} {
			_ = r.rep.Health.Ping(ctx)
			ok = ok && !r.rep.Health.Degraded()
		}
		return ok
	})
	redisreplica.Eventually(t, 10*time.Second, "B to read the model A set during the outage", func() bool { return s.b.settings.Get().AI.EmbeddingModel == "xr-embed-2" })
	// The stream that was running through the outage holds a cross-replica cap slot that could not be released, which
	// counts against its user until its lease passes. Another user's stream runs, and a stop through B reaches it again.
	other := "u-after-the-outage"
	again := startStreamWith(t, s.a, other, s.doc, "xr-blocking", "")
	redisreplica.Eventually(t, 5*time.Second, "the new stream's snapshot readable from B", func() bool {
		r, err := s.b.chat.ResumeStream(ctx, again, 0)
		return err == nil && r != nil
	})
	// The command subscriptions come back on their own backoff; the client's retry on 503 is what bridges the gap.
	redisreplica.Eventually(t, 10*time.Second, "a stop through B to reach the stream on A again", func() bool {
		err := s.b.chat.CancelStreamAs(ctx, other, again)
		if err != nil && !errors.Is(err, service.ErrReplicaUnreachable) {
			t.Fatalf("a stop through B after the outage: %v", err)
		}
		return err == nil
	})
	redisreplica.Eventually(t, 5*time.Second, "the stream on A to end as stopped", func() bool {
		r, err := s.a.chat.ResumeStream(ctx, again, 0)
		return err == nil && r.ErrorCode == service.ChatErrStopped
	})
	redisreplica.Eventually(t, 10*time.Second, "the event stream to carry events across replicas again", func() bool {
		s.b.events.EmitTo(user, "flow:loaded", map[string]string{"name": "after-outage"})
		select {
		case f := <-frames:
			return eventNamed("flow:loaded", "after-outage")(f)
		case <-time.After(100 * time.Millisecond):
			return false
		}
	})
}
