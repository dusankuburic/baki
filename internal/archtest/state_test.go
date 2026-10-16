package archtest

import (
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Per-process state is declared (ADR 0032). With more than one replica behind a load balancer, anything the server
// keeps in a map, a list or a cache is either harmless per replica, or it needs a mechanism that makes the replicas
// agree. TestArch_PerProcessStateDeclared finds the keyed collections that sit beside a lock (a struct with a
// mutex or sync.Map and a map, list or LRU field) and the package-level maps and caches created empty and filled
// later, and requires each to be listed in stateRegistry with its locality and why. A new one fails the test until
// its author has decided what it means for a second replica. Read-only tables built by a composite literal are not
// state and are not asked about.
//
// Out of the scan: internal/testutil (fakes, never linked into the server) and cmd/ (command-line tools, one
// process).

// stateLocality says what a second replica sees of an entry's state.
type stateLocality string

const (
	// stateReplicaLocal: each replica holds its own and that is acceptable, for the reason given (a memo of a pure
	// function, a per-replica limit documented as N times, a connection the replica itself holds). Reasons that begin
	// "known gap" name a cross-replica defect that is not fixed yet.
	stateReplicaLocal stateLocality = "replica-local"
	// stateShared: the replicas agree through a named mechanism (a Redis key or channel, a lease, the database), and
	// a test proves it. The mechanism is the why; the test names the Test function.
	stateShared stateLocality = "shared"
	// stateDatabase: in memory only where there is no server store (the desktop); the database holds it in the
	// server-store profiles. Needs a proving test like shared.
	stateDatabase stateLocality = "database"
	// stateRequestScoped: dies with the request or the turn that made it.
	stateRequestScoped stateLocality = "request-scoped"
	// stateConfig: built at boot or init from configuration or code and read-only afterwards, or a memo of such.
	stateConfig stateLocality = "config"
	// stateDesktopOnly: used only without a server store (the desktop, local mode).
	stateDesktopOnly stateLocality = "desktop-only"
	// stateTestOnly: a fake or a test double that production does not build.
	stateTestOnly stateLocality = "test-only"
	// stateComposite: the type mixes localities; its field entries ("Type.field") say which is which. Needs at least
	// one.
	stateComposite stateLocality = "composite"
)

type stateEntry struct {
	file, ident string
	locality    stateLocality
	why         string // the reason, or for shared and database the mechanism
	test        string // shared and database: a Test function that proves it
}

// stateRegistry lists every piece of per-process state. A type or variable is "File + ident"; a field of a type is
// "Type.field". Entries for mechanisms that are being built in the same phase (the SSE hub, the chat stream registry
// and the budget ledger) say replica-local now and are flipped to shared when those land.
var stateRegistry = []stateEntry{
	// --- internal/ai: provider-side memos and registries
	{"internal/ai/audited.go", "estimateRequestTokensMemo", stateReplicaLocal, "a BPE estimate memoised by request fingerprint (16 entries); another replica computes it again", ""},
	{"internal/ai/budget.go", "BudgetMemo", stateRequestScoped, "one request's daily-usage reads", ""},
	{"internal/ai/circuitbreaker.go", "breakerRegistry", stateReplicaLocal, "a breaker per provider and model, opened by this replica's own failures; a breaker per replica is arguably right (SURVEY_STATE row 18)", ""},
	{"internal/ai/copilot_auth.go", "CopilotAuth", stateReplicaLocal, "Copilot session tokens exchanged per replica; one extra exchange per replica per token lifetime (row 19)", ""},
	{"internal/ai/learned_window.go", "learnedWindows", stateReplicaLocal, "context windows learned from a provider's rejection, 24 h; one wasted rejected call per replica (row 18)", ""},
	{"internal/ai/model_list_cache.go", "modelListCache", stateReplicaLocal, "upstream model listings with a TTL, singleflighted; one upstream call per replica per TTL (row 19). The service-level listing, providers.model-listings, is the invalidated one", ""},
	{"internal/ai/served_model.go", "servedMemo", stateReplicaLocal, "alias to served model, learned from responses; relearned per replica (row 19)", ""},
	{"internal/ai/served_model.go", "servedByResource", stateReplicaLocal, "the index of servedMemo", ""},
	{"internal/ai/spec.go", "providerSpecs", stateConfig, "the provider registry, filled at init", ""},
	{"internal/ai/spec.go", "providerOverrides", stateConfig, "test replacements of a spec (RegisterProvider), never listed", ""},
	{"internal/ai/spec.go", "aliasModelProviders", stateConfig, "derived from the specs at registration", ""},

	// --- internal/api
	{"internal/api/events.go", "EventManager", stateComposite, "the SSE hub: see the field entries", ""},
	{"internal/api/events_fanout.go", "eventFan", stateReplicaLocal, "this replica's subscriber-presence bookkeeping (the users whose local stream count changed); it is rewritten into baki:sse:subs:<user> every 15 s, so another replica reads the shared record, never this map", ""},
	{"internal/api/events.go", "EventManager.clients", stateReplicaLocal, "this replica's open SSE connections, which only this replica can write to. Events reach them from every replica through the SSE fan-out (baki:sse:ev, ADR 0030) when the deployment has Redis", ""},
	{"internal/api/events.go", "EventManager.byUser", stateReplicaLocal, "the per-user index of clients", ""},
	{"internal/api/events.go", "EventManager.sseConnCount", stateShared, "this replica's cap of 10 streams per client key; with more than one replica configured a user's streams on every replica count too (sse.conn-slots, a lease per stream; Redis down: this cap alone)", "TestSSECap_CountsEveryReplicasStreams"},
	{"internal/api/events.go", "EventManager.bufBytes", stateReplicaLocal, "the queued bytes of this replica's own connections", ""},
	{"internal/api/events.go", "EventManager.revokers", stateReplicaLocal, "this replica's streams by access token; a revocation on another replica reaches them as the recheck.sse-revocation message (EventManager.ApplyRemoteRevocation)", ""},
	{"internal/api/flow.go", "uploadLimiter", stateShared, "this replica's cap of 3 uploads in progress per user; with more than one replica configured the user's uploads on every replica count too (flow.upload-slots; Redis down: this cap alone)", "TestUploadCap_CountsEveryReplicasUploads"},
	{"internal/api/handlers.go", "indexShellCache", stateConfig, "the SPA shell's bytes by path: an immutable build artifact", ""},
	{"internal/api/dashboard_params.go", "tzLocCache", stateConfig, "time zones by name: the tz database", ""},
	{"internal/api/middleware/ip.go", "proxySetCache", stateConfig, "parsed trusted-proxy configurations", ""},
	{"internal/api/middleware/metrics.go", "mismatchLogged", stateReplicaLocal, "route and decision pairs already logged: one WARN per pair per process", ""},
	{"internal/api/middleware/ratelimit.go", "inMemoryStore", stateReplicaLocal, "the buckets without Redis and while it is degraded (the limit becomes N times the rate); with Redis they are ratelimit.bucket (row 17)", ""},
	{"internal/api/route/resolver.go", "Resolver", stateConfig, "reads the router's tree, built at boot", ""},
	{"internal/api/route/route.go", "registry", stateConfig, "the route declarations, built at boot", ""},
	{"internal/api/router.go", "Router", stateComposite, "the router: see the field entries", ""},
	{"internal/api/router.go", "Router.usedTickets", stateShared, "ws.ticket (SET NX EX by jti, internal/api/ws_ticket.go) in front of the Postgres-backed blacklist, which stays the authority; the map is the fallback", "TestWSTicket_AReplayThroughAnotherReplicaIsRefused"},
	{"internal/api/migration_runner.go", "MigrationRunner", stateDatabase, "the status of the legacy data migration is read from job_runs (last_detail); the runner's own fields are this replica's view while it runs it", "TestMigrationStatus_AnotherReplicaSeesTheRunAndItsResult"},

	// --- internal/audit
	{"internal/audit/spill.go", "spillStore", stateReplicaLocal, "this replica's spill file (audit-spill-<instance>.jsonl); a dead replica's orphan is adopted by atomic rename and replay is idempotent", ""},
	{"internal/audit/spill.go", "depthGauge", stateReplicaLocal, "the depth of the spill files this recorder holds", ""},

	// --- internal/auth, collaboration, egress, lifecycle, notify
	{"internal/auth/blacklist.go", "TokenBlacklist", stateDesktopOnly, "local mode and tests; the cloud uses CachedBlacklist", ""},
	{"internal/auth/cached_blacklist.go", "CachedBlacklist", stateShared, "the token revocation set: Postgres is the source of truth, baki:auth:revoked fans a revocation out, a 30 s reload and a read through to Postgres back it, and it fails closed", "TestCachedBlacklist_RevocationFansOutAcrossReplicas"},
	{"internal/collaboration/repository.go", "memOrgStore", stateDesktopOnly, "the in-memory organisation store of the desktop", ""},
	{"internal/egress/egress.go", "proxyVerdicts", stateReplicaLocal, "per transport: an operator-allow-listed host's pass (1 min)", ""},
	{"internal/lifecycle/singleton_jobs.go", "jobs", stateConfig, "the job declarations, registered at init (a registry, ADR 0023)", ""},
	{"internal/notify/kinds.go", "registry", stateConfig, "the notifier kinds, registered at init", ""},
	{"internal/notify/notify.go", "Clients", stateReplicaLocal, "one guarded HTTP client per egress class per process: a connection pool", ""},

	// --- internal/platform
	{"internal/platform/eventbus/bus.go", "Bus", stateConfig, "the subscribers and the appliers of remote invalidations, registered at boot; the Replicas hook is installed at boot", ""},

	// --- internal/rag
	{"internal/rag/service.go", "KnowledgeService", stateComposite, "the knowledge base: see the field entries", ""},
	{"internal/rag/service.go", "KnowledgeService.orgLocks", stateShared, "the in-process TryLock is the cheap busy answer; the exclusion across replicas is a lock.Leaser lease per org over the upload and the re-index (knowledge_lock.go)", "TestKnowledgeLock_LeaseIsOnTheOrgsKey"},

	// --- internal/scanner
	{"internal/scanner/scanner.go", "Scanner", stateComposite, "the governance sweep: see the field entries", ""},
	{"internal/scanner/scanner.go", "Scanner.lastScan", stateShared, "scanner.scan (baki:scanner:scan:{flow}, 7 d; ADR 0032 addendum): each flow's last complete scan, read per page (MGET) and written after the scan, when PAD_MAX_REPLICAS > 1. The sweeps of the replicas take turns under the lease, so a flow is analyzed once per change, not once per replica; the map is this replica's copy and the fallback while Redis is down", "TestSharedScans_AFlowOneReplicaScannedIsNotAnalyzedAgainByAnother"},
	{"internal/scanner/scanner.go", "Scanner.lastNotes", stateShared, "the note of a flow's last complete scan travels in the same scanner.scan record: a flow skipped on another replica's record notes it again, which keeps the notice from the retention sweep", "TestSharedScans_AReplicasSkipKeepsTheNoticeOfTheOther"},
	{"internal/scanner/scanner.go", "Scanner.lastSig", stateReplicaLocal, "a pre-check in front of the inbox, which is the deployment-wide de-duplication: an alert's row id is (flow, type, signature), its insert is ON CONFLICT DO NOTHING, and only an inserted alert is delivered, so a replica that never saw an alert delivers nothing again. As on a restart, an alert whose row the retention sweep aged out can be raised again by a replica that never saw it", "TestScanner_AnotherReplicaDoesNotDeliverAnAlertAgain"},
	{"internal/scanner/scanner.go", "Scanner.orgNotifiers", stateRequestScoped, "one sweep's built channels per organisation, reset at the start of every sweep", ""},
	{"internal/scanner/scanner.go", "Scanner.ruleDigests", stateRequestScoped, "one sweep's rule-set digest per organisation", ""},
	{"internal/scanner/scanner.go", "Scanner.orgRosters", stateRequestScoped, "one sweep's member lists per organisation", ""},
	{"internal/scanner/scanner.go", "Scanner.ruleSets", stateRequestScoped, "one sweep's recorded rule sets by digest", ""},
	{"internal/scanner/scanner.go", "Scanner.notices", stateRequestScoped, "one sweep's \"new rules\" notices; the database's primary key decides which notice is new, across replicas", ""},

	// --- internal/service
	{"internal/service/ai_budget_redis.go", "redisLedger", stateReplicaLocal, "this replica's own held reservations, mirrored into the baki:ai:inflight:* hashes every second; other replicas read the hashes, never this map", ""},
	{"internal/service/ai_budget.go", "memLedger", stateReplicaLocal, "the in-flight spend of this replica's streams, used when there is no Redis. With Redis the spend lives in the baki:ai:inflight:* hashes (internal/redisx/inflight.go, ai_budget_redis.go), which fail closed (ADR 0028 addendum, ai-providers.md)", ""},
	{"internal/service/analysis.go", "recordClaims", stateShared, "analysis.claim (baki:analysis:claim:{tenant}:{flow}, 7 d; ADR 0032 addendum): the claimed run is swapped in with SET ... GET when PAD_MAX_REPLICAS > 1, so a run is recorded (trend row, analysis webhook) once wherever it is analyzed; the LRU is the fallback while Redis is down", "TestSharedAnalysis_ARunRecordedOnOneReplicaIsNotFreshOnAnother"},
	{"internal/service/analysis.go", "reportLRU", stateShared, "analysis.pair (baki:analysis:pair:{tenant}:{flow}, 24 h; ADR 0032 addendum): the flow's last two runs and their reports (gzip, at most 512 KiB each; none for a sweep's run) when PAD_MAX_REPLICAS > 1, so the export and the diff on any replica answer with the deployment's runs; the LRU serves the runs this replica holds and is the fallback (Redis down, past the TTL, a report over the cap)", "TestSharedAnalysis_AnotherReplicaServesTheCurrentRunAndDiffsAgainstThePrevious"},
	{"internal/service/authz.go", "flowAuthzMemo", stateRequestScoped, "one handler's authorization decisions about one flow", ""},
	{"internal/service/azure_models_runtime.go", "azureListingRefresher", stateReplicaLocal, "the queue of this replica's stale Azure listings to refresh", ""},
	{"internal/redisx/command.go", "CommandBus", stateReplicaLocal, "the calls this replica is waiting on (reply channel waiters) and its handler slots; the commands and replies themselves travel on baki:chat:cmd:<instance> and baki:chat:reply:<instance> (ADR 0033)", ""},
	{"internal/redisx/slots.go", "SlotCap", stateReplicaLocal, "the cap slots this replica holds, whose leases it refreshes; the count across the replicas is the per-user sorted set in Redis, which other replicas read, never this map", ""},
	{"internal/redisx/command_auth.go", "nonceSet", stateReplicaLocal, "the nonces of the signed command frames this replica accepted in the last minute (replay protection): a frame is addressed to one replica, so only that replica needs to remember it (ADR 0033 addendum)", ""},
	{"internal/service/chat.go", "ChatService", stateComposite, "the chat service: see the field entries", ""},
	{"internal/service/chat.go", "ChatService.activeStreams", stateReplicaLocal, "the streams this replica runs, which only it can stop. A stop, an approval decision and a reload on another replica reach the owner through the resume snapshot's replica field and baki:chat:cmd:<instance> (ADR 0033)", ""},
	{"internal/service/chat.go", "ChatService.finishedStreams", stateReplicaLocal, "the finished streams of this replica for two minutes (resume); another replica resumes from the shared resume snapshot, see activeStreams", ""},
	{"internal/service/chat.go", "ChatService.pendingFixDecisions", stateReplicaLocal, "the approval cards of this replica's streams; the pending key and the first-decision claim (baki:chat:approval, baki:chat:decision) let another replica decide, see activeStreams", ""},
	{"internal/service/chat.go", "ChatService.convMutexes", stateReplicaLocal, "an in-process reducer of contention on one conversation; correctness across replicas is the conversation version (compare-and-swap, ADR 0031)", ""},
	{"internal/service/chat.go", "ChatService.streamCapMu", stateShared, "the per-caller cap of 3 streams and the clientStreamId check on this replica; with more than one replica configured both also hold across the replicas (chat.stream-slots, chat.stream-id; Redis down: this replica's alone)", "TestStreamCap_CountsEveryReplicasStreams"},
	{"internal/service/chat_loop_memory.go", "loopToolMemory", stateRequestScoped, "one chat turn's tool loop", ""},
	{"internal/service/chat_scrub_cache.go", "scrubMemo", stateReplicaLocal, "the storage behind chat.history-scrubs: a pure function of the caller's text", ""},
	{"internal/service/chat_scrub_cache.go", "tokenCountMemo", stateReplicaLocal, "the storage behind chat.token-counts: a pure function of the text", ""},
	{"internal/service/chat_stream_control.go", "streamCtl", stateReplicaLocal, "one in-flight stream's buffer and cancellation, on the replica that runs it; see ChatService.activeStreams", ""},
	{"internal/service/chat_turn_memo.go", "turnMemo", stateRequestScoped, "one chat turn's preferences and org-role lookups", ""},
	{"internal/service/dashboard_analytics.go", "anaStore", stateShared, "redis-inval: dashboard.analytics is dropped on every replica by the flow change or analysis that made it stale (baki:inval)", "TestDashboard_AppliesRemoteInvalidations"},
	{"internal/service/flow.go", "FlowService", stateComposite, "the flow service: see the field entries", ""},
	{"internal/service/flow.go", "FlowService.uploadIDs", stateShared, "flow.upload-id (SET NX EX in Redis, claimed per caller and content) under the local memo, which is the fallback", "TestUploadIDs_AnIdenticalReuploadOnAnotherReplicaUpdatesTheSameFlow"},
	{"internal/service/flow.go", "FlowService.patchLocks", stateReplicaLocal, "serialises read-modify-write patches per absolute path: a desktop file's lock, and a desktop file lives on one machine", ""},
	{"internal/service/flow_snapshots.go", "snapshotStore", stateDatabase, "the undo ring of stored flows is the flow_snapshots table (WithSnapshotStore); this memory ring serves desktop file flows only", "TestSnapshotStore_TwoReplicasShareTheRing"},
	{"internal/service/health.go", "blobCheckedAt", stateReplicaLocal, "one timestamp: the last successful blob probe (15 s)", ""},
	{"internal/service/org_rules.go", "loggedOnce", stateReplicaLocal, "the stored-rule problems already logged: once per content per process", ""},
	{"internal/service/providers.go", "ProviderService", stateShared, "redis-inval: providers.model-listings is dropped on every replica by the credential change that made it stale (baki:inval); a lost message is bounded by the 10 min TTL", "TestModelCache_AppliesRemoteInvalidations"},
	{"internal/service/rule_profile.go", "profileStore", stateShared, "redis-inval: rules.profiles and rules.deployment-profile are dropped on every replica by the settings, rule or membership change that made them stale (baki:inval)", "TestRuleProfiles_ApplyRemoteInvalidations"},

	// --- internal/sso, storage, websocket
	{"internal/sso/client.go", "providerFetched", stateReplicaLocal, "one value: the IdP's discovery document (15 min)", ""},
	{"internal/storage/azure_registry_mem.go", "MemoryAzureRegistry", stateTestOnly, "backs the test fake, nothing in production", ""},
	{"internal/storage/settings.go", "SettingsStore", stateDesktopOnly, "the settings file of the desktop and local mode; the cloud reads the database", ""},
	{"internal/storage/settingsdoc/settingsdoc.go", "fieldCache", stateConfig, "struct fields by reflect.Type", ""},
	{"internal/websocket/hub.go", "Hub", stateComposite, "the WebSocket hub: see the field entries", ""},
	{"internal/websocket/hub.go", "Hub.rooms", stateReplicaLocal, "the sockets this replica holds; broadcasts and presence reach the other replicas' sockets through the ws.room and ws.presence backplane (Redis), and access changes through recheck.ws-access", "TestBroadcast_PropagatesAcrossReplicas"},
	{"internal/websocket/hub.go", "Hub.connsPerUser", stateShared, "this hub's cap of 10 sockets per user; with more than one replica configured the user's sockets on every replica count too (ws.conn-slots; Redis down: this cap alone)", "TestConnCap_CountsEveryReplicasSockets"},
	{"internal/websocket/presence_writer.go", "presenceWriter", stateReplicaLocal, "the queue of this replica's presence writes to the shared hash", ""},
}

// stateOutOfScope are the paths the scan does not look at, with why.
var stateOutOfScope = []struct{ prefix, why string }{
	{"internal/testutil/", "fakes and harnesses, never linked into the server"},
	{"cmd/", "command-line tools: one process"},
}

type stateHit struct{ file, ident, kind string }

// stateHits finds the keyed collections beside a lock, and the maps and caches made empty at package level.
func stateHits(t *testing.T) []stateHit {
	t.Helper()
	var out []stateHit
files:
	for _, f := range sourceFiles(t) {
		for _, o := range stateOutOfScope {
			if strings.HasPrefix(f.path, o.prefix) {
				continue files
			}
		}
		sync := importNames(f.ast, func(p string) (string, bool) { return "sync", p == "sync" })
		atom := importNames(f.ast, func(p string) (string, bool) { return "atomic", p == "sync/atomic" })
		lst := importNames(f.ast, func(p string) (string, bool) { return "list", p == "container/list" })
		lru := importNames(f.ast, func(p string) (string, bool) {
			return "lru", p == "github.com/hashicorp/golang-lru" || strings.HasPrefix(p, "github.com/hashicorp/golang-lru/")
		})
		sel := func(e ast.Expr, names map[string]string, want ...string) bool {
			if st, ok := e.(*ast.StarExpr); ok {
				e = st.X
			}
			if ix, ok := e.(*ast.IndexExpr); ok {
				e = ix.X
			}
			s, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := s.X.(*ast.Ident)
			if !ok || names[x.Name] == "" {
				return false
			}
			for _, w := range want {
				if w == "*" || s.Sel.Name == w {
					return true
				}
			}
			return false
		}
		isLock := func(e ast.Expr) bool {
			if at, ok := e.(*ast.ArrayType); ok {
				e = at.Elt
			}
			return sel(e, sync, "Mutex", "RWMutex", "Map") || sel(e, atom, "*")
		}
		isColl := func(e ast.Expr) bool {
			if st, ok := e.(*ast.StarExpr); ok {
				e = st.X
			}
			if _, ok := e.(*ast.MapType); ok {
				return true
			}
			return sel(e, sync, "Map") || sel(e, lst, "List") || sel(e, lru, "*")
		}
		for _, d := range f.ast.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					st, ok := s.Type.(*ast.StructType)
					if !ok {
						continue
					}
					lock, coll := false, false
					for _, fld := range st.Fields.List {
						lock = lock || isLock(fld.Type)
						coll = coll || isColl(fld.Type)
					}
					if lock && coll {
						out = append(out, stateHit{f.path, s.Name.Name, "type"})
					}
				case *ast.ValueSpec:
					if gen.Tok != token.VAR {
						continue
					}
					for i, name := range s.Names {
						if name.Name == "_" {
							continue
						}
						hit := s.Type != nil && isColl(s.Type)
						if i < len(s.Values) {
							switch v := s.Values[i].(type) {
							case *ast.CallExpr:
								if id, ok := v.Fun.(*ast.Ident); ok && len(v.Args) > 0 && (id.Name == "make" || id.Name == "new") && isColl(v.Args[0]) {
									hit = true
								}
							case *ast.CompositeLit:
								if isColl(v.Type) && len(v.Elts) == 0 {
									hit = true
								}
							}
						}
						if hit {
							out = append(out, stateHit{f.path, name.Name, "var"})
						}
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].ident < out[j].ident
	})
	return out
}

// testFuncNames is every Test function declared in a _test.go file of the repository's Go modules.
func testFuncNames(t *testing.T) map[string]bool {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	root := repoRoot(t)
	names := map[string]bool{}
	for _, dir := range []string{"internal", "cmd", "core"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range decl.FindAllSubmatch(b, -1) {
				names[string(m[1])] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return names
}

// TestArch_PerProcessStateDeclared: every keyed collection beside a lock, and every empty package-level map or cache,
// is in stateRegistry; every entry says why (and, shared or database, names the mechanism and a test that exists).
func TestArch_PerProcessStateDeclared(t *testing.T) {
	registered := map[string]bool{}
	for _, e := range stateRegistry {
		registered[e.file+"\x00"+e.ident] = true
	}
	hits := stateHits(t)
	if len(hits) < 40 {
		t.Fatalf("found %d pieces of state, far fewer than the registry lists: the AST walk is broken", len(hits))
	}
	for _, h := range hits {
		if !registered[h.file+"\x00"+h.ident] {
			t.Errorf("%s: %s %s holds keyed state in this process and is not in stateRegistry (internal/archtest/state_test.go). "+
				"Decide what a second replica sees of it: replica-local with a reason, shared through a mechanism with a proving test, "+
				"request-scoped, config or desktop-only", h.file, h.kind, h.ident)
		}
	}

	tests := testFuncNames(t)
	seen := map[string]bool{}
	fields := map[string]int{} // type -> its field entries
	for _, e := range stateRegistry {
		id := e.file + " " + e.ident
		if seen[id] {
			t.Errorf("stateRegistry lists %s twice", id)
		}
		seen[id] = true
		if typ, _, dotted := strings.Cut(e.ident, "."); dotted {
			fields[e.file+"\x00"+typ]++
		}
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s has no reason: say why (replica-local) or by what mechanism (shared)", id)
		}
		switch e.locality {
		case stateReplicaLocal, stateRequestScoped, stateConfig, stateDesktopOnly, stateTestOnly, stateComposite:
		case stateShared, stateDatabase:
			if e.test == "" {
				t.Errorf("%s is %s through %q but names no test that proves it", id, e.locality, e.why)
			}
		default:
			t.Errorf("%s has the unknown locality %q", id, e.locality)
		}
		if e.test != "" && !tests[e.test] {
			t.Errorf("%s names the test %s, which does not exist", id, e.test)
		}
	}
	for _, e := range stateRegistry {
		if e.locality == stateComposite && fields[e.file+"\x00"+e.ident] == 0 {
			t.Errorf("%s %s is composite but has no field entry (Type.field) saying which part is which", e.file, e.ident)
		}
	}
}

// TestArch_StateRegistryEntriesStillExist: each entry is still where it says (the file exists and still has the
// identifier, a field's name for "Type.field"). A moved or renamed piece of state is reviewed again.
func TestArch_StateRegistryEntriesStillExist(t *testing.T) {
	files := map[string]*ast.File{}
	for _, f := range sourceFiles(t) {
		files[f.path] = f.ast
	}
	for _, e := range stateRegistry {
		f, ok := files[e.file]
		if !ok {
			t.Errorf("%s (%s) is gone: remove it from stateRegistry, or update the entry", e.file, e.ident)
			continue
		}
		_, name, dotted := strings.Cut(e.ident, ".")
		if !dotted {
			name = e.ident
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return !found
		})
		if !found {
			t.Errorf("%s no longer has %s: review the state (update stateRegistry)", e.file, e.ident)
		}
	}
}

// TestArch_StateRegistryCoversTheSurveyedState: the pieces of state the Phase 5b surveys named are listed, whatever
// the scan finds, so deleting an entry (or renaming the thing it describes) is a visible change.
func TestArch_StateRegistryCoversTheSurveyedState(t *testing.T) {
	have := map[string]bool{}
	for _, e := range stateRegistry {
		have[e.ident] = true
	}
	for _, want := range []string{
		"ChatService.activeStreams", "ChatService.finishedStreams", "ChatService.pendingFixDecisions", "ChatService.convMutexes",
		"Router.usedTickets", "EventManager.sseConnCount", "Hub.connsPerUser", "EventManager.revokers", "Hub.rooms", "EventManager.bufBytes",
		"KnowledgeService.orgLocks", "Scanner", "memLedger", "spillStore", "snapshotStore", "recordClaims", "reportLRU",
		"MigrationRunner", "FlowService.uploadIDs", "inMemoryStore", "breakerRegistry", "learnedWindows", "CachedBlacklist",
	} {
		if !have[want] {
			t.Errorf("stateRegistry lost %s, which SURVEY_STATE or the realtime survey lists", want)
		}
	}
	if len(stateRegistry) < 60 {
		t.Errorf("stateRegistry has %d entries, far fewer than before", len(stateRegistry))
	}
}
