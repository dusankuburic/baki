package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/reqtx"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/cache"
	"pad-core/models"
)

// memberOnlyRulesBackend shows an org's custom rules the way row-level security does: in a request's scope only to
// a member caller, without a scope (the pool: a background job, a route exempt from the scope) always.
type memberOnlyRulesBackend struct {
	*testutil.FakeBackend
	members map[string]bool
	reads   atomic.Int32
	// gate, when set, holds every read until closed; entered receives each held read's start.
	gate    chan struct{}
	entered chan struct{}
	// afterRead, when set, runs once a read has seen the rows, before it returns them.
	afterRead func()
}

func (b *memberOnlyRulesBackend) ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgCustomRule, error) {
	b.reads.Add(1)
	if b.gate != nil {
		b.entered <- struct{}{}
		<-b.gate
	}
	rows, err := b.FakeBackend.ListOrgCustomRules(ctx, orgID, enabledOnly)
	if reqtx.ReadsFiltered(ctx) {
		if a := auth.ActorFromContext(ctx); a == nil || !b.members[a.UserID] {
			rows = nil
		}
	}
	if b.afterRead != nil {
		b.afterRead()
	}
	return rows, err
}

// OrgMemberRole answers from members, the same set the rows are shown to.
func (b *memberOnlyRulesBackend) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	if b.members[userID] {
		return auth.RoleMember, true, nil
	}
	return "", false, nil
}

// testScope stands in for a request's row-level-security scope; its reads stop being filtered once it "failed open".
type testScope struct{ failedOpen atomic.Bool }

// on returns ctx in the scope.
func (s *testScope) on(ctx context.Context) context.Context {
	return reqtx.WithReadFilter(ctx, func() bool { return !s.failedOpen.Load() })
}

// inScope is userID's request on a route in the scope.
func inScope(userID string) context.Context {
	return new(testScope).on(onPool(userID))
}

// onPool is userID's request on a route exempt from the scope: its reads run on the pool.
func onPool(userID string) context.Context {
	return auth.WithActor(context.Background(), userActor(userID, auth.RoleMember))
}

func newMemberOnlyResolver(t *testing.T) (*RuleProfileResolver, *memberOnlyRulesBackend) {
	t.Helper()
	b := &memberOnlyRulesBackend{FakeBackend: testutil.NewFakeBackend(), members: map[string]bool{"member": true}}
	storeRule(t, b.FakeBackend, "org-1", "org-rule", "^SET$", true)
	return NewRuleProfileResolver(b, deploymentSettings(nil), nil), b
}

// B4: the profile was cached per org whoever resolved it, so a non-member collaborator's resolve (whose scope
// shows no custom rules) hid the org's rules from every member for the TTL, and a member's resolve showed them
// to the non-member. Each reader now gets exactly what its own scope shows, in either order.
func TestRuleProfileResolver_EachReaderGetsItsOwnView(t *testing.T) {
	r, _ := newMemberOnlyResolver(t)
	outsider := inScope("outsider")
	member := inScope("member")
	system := auth.WithActor(context.Background(), auth.SystemActor("scanner"))

	if hasRule(r.Resolve(outsider, "org-1").Rules, "org-rule") {
		t.Fatal("premise: a non-member's scope shows the org's custom rules")
	}
	if !hasRule(r.Resolve(member, "org-1").Rules, "org-rule") {
		t.Error("a member was served the non-member's profile: the org's custom rules are missing")
	}
	if hasRule(r.Resolve(outsider, "org-1").Rules, "org-rule") {
		t.Error("a non-member was served a member's profile: the org's custom rules leaked")
	}
	if !hasRule(r.Resolve(system, "org-1").Rules, "org-rule") || !hasRule(r.Resolve(context.Background(), "org-1").Rules, "org-rule") {
		t.Error("a background job's resolve (every row) is missing the org's custom rules")
	}
	if r.cachedFor("org-1") == nil {
		t.Error("the org's own entry (background jobs) was not cached")
	}
}

// Every reader's entry is cached on its own, and an org's invalidation drops all of them.
func TestRuleProfileResolver_ReaderEntriesCacheAndInvalidateTogether(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	ctxs := []context.Context{inScope("outsider"), inScope("member"), context.Background()}
	for _, ctx := range ctxs {
		r.Resolve(ctx, "org-1")
	}
	b.reads.Store(0)
	for _, ctx := range ctxs {
		r.Resolve(ctx, "org-1")
	}
	if n := b.reads.Load(); n != 0 {
		t.Errorf("warm resolves read the custom rules %d times, want 0 (each reader's entry cached)", n)
	}
	r.Invalidate("org-2")
	for _, ctx := range ctxs {
		r.Resolve(ctx, "org-1")
	}
	if n := b.reads.Load(); n != 0 {
		t.Errorf("another org's invalidation dropped %d of org-1's entries", n)
	}
	r.Invalidate("org-1")
	for _, ctx := range ctxs {
		r.Resolve(ctx, "org-1")
	}
	if n := b.reads.Load(); n != int32(len(ctxs)) {
		t.Errorf("after the org's invalidation %d readers re-resolved, want all %d", n, len(ctxs))
	}
}

// Concurrent cold resolves share a flight per reader, never across readers: a member joining a non-member's
// flight would receive the non-member's view. The member's resolve is held in its read while the others start.
func TestRuleProfileResolver_FlightsNeverJoinAcrossReaders(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	b.gate, b.entered = make(chan struct{}), make(chan struct{}, 16)
	var wg sync.WaitGroup
	results := make([]bool, 8)
	resolve := func(i int, user string) {
		defer wg.Done()
		results[i] = hasRule(r.Resolve(inScope(user), "org-1").Rules, "org-rule")
	}
	wg.Add(1)
	go resolve(0, "member")
	<-b.entered // the member's flight is reading
	for i := 1; i < len(results); i++ {
		user := "outsider"
		if i%2 == 0 {
			user = "member"
		}
		wg.Add(1)
		go resolve(i, user)
	}
	// The outsiders' own flight reads too; joining the member's flight instead would never get here.
	select {
	case <-b.entered:
	case <-time.After(2 * time.Second):
		t.Error("the non-members' resolves joined the member's flight instead of reading their own view")
	}
	close(b.gate)
	wg.Wait()
	for i, got := range results {
		if want := i%2 == 0; got != want {
			t.Errorf("caller %d (member=%v) got the org's custom rules = %v", i, want, got)
		}
	}
	if n := b.reads.Load(); n != 2 {
		t.Errorf("%d custom-rule reads for two readers' concurrent cold resolves, want 2 (one per reader)", n)
	}
}

// A caller without a user id in a scope has no entry of its own: resolved every time, never cached under another
// reader; nor is a scope without a calling user (whose view it shows is unknown).
func TestRuleProfileResolver_CallerWithoutUserIsNotCached(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no user id":   new(testScope).on(auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorUser})),
		"system actor": new(testScope).on(auth.WithActor(context.Background(), auth.SystemActor("scanner"))),
	} {
		t.Run(name, func(t *testing.T) {
			r, b := newMemberOnlyResolver(t)
			r.Resolve(ctx, "org-1")
			r.Resolve(ctx, "org-1")
			if n := b.reads.Load(); n != 2 {
				t.Errorf("read the custom rules %d times for two resolves, want 2 (uncached)", n)
			}
			if r.cachedFor("org-1") != nil {
				t.Error("filled the org's own entry")
			}
		})
	}
}

// A resolve on the pool (a route exempt from the scope) reads every row, whoever asks, so it is the org's own view
// and never the caller's entry: a non-member's pool resolve was cached as their own and then served on the scoped
// routes (baseline, policy), showing them the org's custom rules there; the other order hid the rules from their
// pool-route analyses.
func TestRuleProfileResolver_PoolAndScopedReadsKeepApart(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	if !hasRule(r.Resolve(onPool("outsider"), "org-1").Rules, "org-rule") {
		t.Fatal("premise: a pool read shows every row")
	}
	if hasRule(r.Resolve(inScope("outsider"), "org-1").Rules, "org-rule") {
		t.Error("a non-member's scoped resolve was served their pool resolve, with the org's custom rules")
	}
	if !hasRule(r.Resolve(onPool("outsider"), "org-1").Rules, "org-rule") {
		t.Error("a pool resolve was served the non-member's scoped view")
	}
	b.reads.Store(0)
	r.Resolve(context.Background(), "org-1")
	r.Resolve(onPool("member"), "org-1")
	if n := b.reads.Load(); n != 0 {
		t.Errorf("pool resolves read the custom rules %d times after another pool resolve, want 0 (the org's entry)", n)
	}
}

// A fail-open scope whose start failed while the resolve read ran on the pool: what it read is not the caller's
// scoped view, so it is not cached as theirs.
func TestRuleProfileResolver_FailedOpenScopeIsNotCachedAsTheCallers(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	b.gate, b.entered = make(chan struct{}), make(chan struct{}, 1)
	scope := &testScope{}
	done := make(chan *RuleProfile)
	go func() { done <- r.Resolve(scope.on(onPool("outsider")), "org-1") }()
	<-b.entered
	scope.failedOpen.Store(true)
	close(b.gate)
	<-done
	b.gate = nil
	if hasRule(r.Resolve(inScope("outsider"), "org-1").Rules, "org-rule") {
		t.Error("a failed-open resolve's pool view was cached as the non-member's scoped entry")
	}
}

// Membership decides what a member's entry shows, so a membership change drops the org's entries: a removed
// member kept the org's custom rules for the TTL, and a new one went without them.
func TestSubscribeRuleProfiles_MembershipChangeDropsTheOrgsEntries(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, r)
	ctx := context.Background()

	if !hasRule(r.Resolve(inScope("member"), "org-1").Rules, "org-rule") {
		t.Fatal("premise: a member's scope shows the org's custom rules")
	}
	b.members["member"] = false
	bus.Publish(ctx, events.OrgMemberChanged{OrgID: "org-1", UserID: "member", Kind: events.OrgMemberRemoved})
	if hasRule(r.Resolve(inScope("member"), "org-1").Rules, "org-rule") {
		t.Error("a removed member was still served the org's custom rules from their entry")
	}

	if hasRule(r.Resolve(inScope("joiner"), "org-1").Rules, "org-rule") {
		t.Fatal("premise: a non-member's scope shows the org's custom rules")
	}
	b.members["joiner"] = true
	bus.Publish(ctx, events.OrgMemberChanged{OrgID: "org-1", UserID: "joiner", Kind: events.OrgMemberAdded})
	if !hasRule(r.Resolve(inScope("joiner"), "org-1").Rules, "org-rule") {
		t.Error("a new member was served their pre-join entry, without the org's custom rules")
	}
}

// The resolver's own store counts on its spec: a miss and a store per cold resolve, a hit per warm one, and a
// deleted eviction per entry an invalidation drops; the deployment profile counts on its own spec.
func TestRuleProfileResolver_CountsOnItsSpecs(t *testing.T) {
	r, _ := newMemberOnlyResolver(t)
	ctx := context.Background()
	before, depBefore := ruleProfiles.Info().Stats, deploymentProfile.Info().Stats
	r.Resolve(ctx, "org-1")
	r.Resolve(ctx, "org-1")
	r.Resolve(ctx, "")
	r.Invalidate("org-1")
	after, depAfter := ruleProfiles.Info().Stats, deploymentProfile.Info().Stats
	if got := [4]uint64{after.Hits - before.Hits, after.Misses - before.Misses, after.Stores - before.Stores,
		after.Evictions[cache.EvictDeleted] - before.Evictions[cache.EvictDeleted]}; got != [4]uint64{1, 1, 1, 1} {
		t.Errorf("rules.profiles counted hits, misses, stores, deletions %v, want [1 1 1 1]", got)
	}
	if depAfter.Misses-depBefore.Misses != 1 || depAfter.Stores-depBefore.Stores != 1 || depAfter.Hits != depBefore.Hits {
		t.Errorf("rules.deployment-profile counted %+v -> %+v, want one miss and one store", depBefore, depAfter)
	}
}

// A membership change that commits while a resolve of the org is in flight (read before, stored after) refuses
// that resolve's store: a member removed then would keep the org's custom rules for the TTL.
func TestSubscribeRuleProfiles_MembershipChangeFencesResolvesInFlight(t *testing.T) {
	r, b := newMemberOnlyResolver(t)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, r)
	b.afterRead = func() {
		b.afterRead = nil
		b.members["member"] = false
		bus.Publish(context.Background(), events.OrgMemberChanged{OrgID: "org-1", UserID: "member", Kind: events.OrgMemberRemoved})
	}
	if !hasRule(r.Resolve(inScope("member"), "org-1").Rules, "org-rule") {
		t.Fatal("premise: the resolve read while the caller was a member")
	}
	if hasRule(r.Resolve(inScope("member"), "org-1").Rules, "org-rule") {
		t.Error("a resolve that read before the member's removal was cached: the removed member keeps the org's custom rules")
	}
}

// A non-member's scoped view is marked Narrowed (its analyses must not stand in for the org's); a member's, the
// pool's and a background job's are the org's own.
func TestRuleProfileResolver_NarrowedMarksOutsidersOnly(t *testing.T) {
	r, _ := newMemberOnlyResolver(t)
	for _, c := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"non-member in the scope", inScope("outsider"), true},
		{"member in the scope", inScope("member"), false},
		{"non-member on the pool", onPool("outsider"), false},
		{"background job", auth.WithActor(context.Background(), auth.SystemActor("scanner")), false},
	} {
		for range 2 { // cold, then cached
			if got := r.Resolve(c.ctx, "org-1").Narrowed; got != c.want {
				t.Errorf("%s: Narrowed = %t, want %t", c.name, got, c.want)
			}
		}
	}
	if r.Resolve(inScope("outsider"), "").Narrowed {
		t.Error("the deployment profile (no org) is nobody's narrowed view")
	}
}

// caches-1: the report pair, the dashboard claim and the history are the flow's, read by members' exports, diffs
// and chat. A non-member collaborator's run on a scoped route (policy evaluation, baselines, comments) sees no org
// custom rules; it used to become the flow's current report, so members exported and diffed without the org's
// rules, and evicted the org's cached report.
func TestAnalyzeFlow_NarrowedRunIsTheReadersAlone(t *testing.T) {
	r, _ := newMemberOnlyResolver(t)
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{AuthEnabled: true, Profiles: r})
	if err != nil {
		t.Fatal(err)
	}
	doc := makeAnalyzableDoc("flow-1",
		models.Block{ID: "b1", SubflowID: "main", Name: "Set X", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 1})
	doc.OwnerID, doc.OrganizationID = "member", "org-1"
	countOrgRule := func(rep *models.AnalysisReport) int {
		n := 0
		for _, f := range rep.Findings {
			if f.RuleID == "org-rule" {
				n++
			}
		}
		return n
	}

	memberRun, fresh, err := svc.AnalyzeFlowFresh(onPool("member"), doc)
	if err != nil || !fresh || countOrgRule(memberRun) == 0 {
		t.Fatalf("premise: the member's run (fresh %t, err %v) finds the org rule %d times", fresh, err, countOrgRule(memberRun))
	}
	outsiderRun, err := svc.AnalyzeFlow(inScope("outsider"), doc)
	if err != nil || countOrgRule(outsiderRun) != 0 {
		t.Fatalf("premise: the non-member's view has no org rules (found %d, err %v)", countOrgRule(outsiderRun), err)
	}
	if again, err := svc.AnalyzeFlow(inScope("outsider"), doc); err != nil || again != outsiderRun {
		t.Errorf("the non-member's run is cached as its own (same report %t, err %v)", again == outsiderRun, err)
	}
	if cur, ok := svc.CurrentReport(doc); !ok || cur != memberRun {
		t.Error("the non-member's run replaced the flow's current report")
	}
	if _, has := svc.PreviousReport(doc); has {
		t.Error("the non-member's run shifted the flow's previous report")
	}
	if again, fresh, err := svc.AnalyzeFlowFresh(inScope("member"), doc); err != nil || again != memberRun || fresh {
		t.Errorf("the member's next run: cached %t, fresh %t, err %v; want the org's cached report, already recorded", again == memberRun, fresh, err)
	}
	if _, fresh, _ := svc.AnalyzeFlowFresh(inScope("outsider"), doc); fresh {
		t.Error("a non-member's run claimed the flow's dashboard record")
	}
}

// s2-1: a shared write built from an analysis (POST /api/analysis/baseline/set) asks ReadsOrgView first: a
// non-member's scoped view must not become the org flow's baseline.
func TestAnalysisService_ReadsOrgView(t *testing.T) {
	r, _ := newMemberOnlyResolver(t)
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{AuthEnabled: true, Profiles: r})
	if err != nil {
		t.Fatal(err)
	}
	orgFlow := &models.FlowDocument{ID: "flow-1", OwnerID: "member", OrganizationID: "org-1"}
	personal := &models.FlowDocument{ID: "flow-2", OwnerID: "outsider"}
	for _, c := range []struct {
		name string
		ctx  context.Context
		doc  *models.FlowDocument
		want bool
	}{
		{"non-member in the scope", inScope("outsider"), orgFlow, false},
		{"member in the scope", inScope("member"), orgFlow, true},
		{"non-member on the pool", onPool("outsider"), orgFlow, true},
		{"a flow without an org", inScope("outsider"), personal, true},
	} {
		if got := svc.ReadsOrgView(c.ctx, c.doc); got != c.want {
			t.Errorf("%s: ReadsOrgView = %t, want %t", c.name, got, c.want)
		}
	}
}
