package analyzer

import (
	"context"
	"sync"
	"testing"
	"time"

	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

// walkGate is a block rule that counts walks (each walk builds its own RuleContext) and holds every walk at its
// first check until release, so a test can line concurrent callers up behind a walk in progress.
type walkGate struct {
	mu      sync.Mutex
	walks   map[*RuleContext]bool
	entered chan struct{} // one send per walk that reached the rule
	release chan struct{}
	once    sync.Once
}

func newWalkGate() *walkGate {
	return &walkGate{walks: map[*RuleContext]bool{}, entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (*walkGate) ID() string                       { return "test-walk-gate" }
func (*walkGate) Name() string                     { return "Test walk gate" }
func (*walkGate) Description() string              { return "" }
func (*walkGate) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (*walkGate) Category() string                 { return "test" }
func (g *walkGate) Check(_ *models.Block, ctx *RuleContext) []models.Finding {
	g.mu.Lock()
	first := !g.walks[ctx]
	g.walks[ctx] = true
	g.mu.Unlock()
	if first {
		g.entered <- struct{}{}
	}
	<-g.release
	return nil
}

func (g *walkGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *walkGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.walks)
}

// await waits for the next walk to reach the gate; false after d.
func (g *walkGate) await(d time.Duration) bool {
	select {
	case <-g.entered:
		return true
	case <-time.After(d):
		return false
	}
}

// B1.2: concurrent identical analyses dedup to ONE full walk (singleflight).
func TestCachedAnalyzer_SingleflightDedup(t *testing.T) {
	src := "#Region \"Main\"\n    SET A TO 1\n    SET B TO 2\n#EndRegion\n"
	doc, err := parser.ParseText(src, "SFDedup.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := newTestAnalyzer()
	gate := newWalkGate()
	defer gate.open()
	rules := append(AllRules(), gate)

	const callers = 8
	misses := func() uint64 { return reportSpec.Info().Stats.Misses }
	before := misses()
	reports := make([]*models.AnalysisReport, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i] = a.Analyze(context.Background(), localIn(doc, rules, nil, nil))
		}()
	}
	if !gate.await(10 * time.Second) {
		t.Fatal("no walk started")
	}
	// Hold the first walk until every caller has missed the cache: each now joins the flight (or, if it reaches the
	// flight after the walk, finds the stored report). Without the flight each one walks.
	for deadline := time.Now().Add(10 * time.Second); misses()-before < callers; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d callers missed the cache", misses()-before, callers)
		}
	}
	gate.open()
	wg.Wait()

	if n := gate.count(); n != 1 {
		t.Errorf("%d walks for %d concurrent identical requests, want 1", n, callers)
	}
	for i, r := range reports {
		if r == nil || r != reports[0] {
			t.Errorf("caller %d got report %p, want the shared %p", i, r, reports[0])
		}
	}
}

// The flight is per FULL report key: concurrent requests the key separates (another tenant, another parse of the
// same text) each walk and get their own report, even for the same flow while its walk is in progress.
func TestCachedAnalyzer_ConcurrentKeysNeverShareAWalk(t *testing.T) {
	for _, c := range []struct {
		name             string
		sameDoc          bool
		tenant1, tenant2 cache.Part
	}{
		{"same document, two tenants", true, cache.Org("o1"), cache.User("u1")},
		{"two parses, two tenants", false, cache.Org("victim-org"), cache.User("attacker")},
		{"two parses, one tenant", false, cache.Org("o1"), cache.Org("o1")},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestAnalyzer()
			gate := newWalkGate()
			defer gate.open()
			rules := append(AllRules(), gate)
			first := parseRaw(t, rawFlow, "first-project")
			second := first
			if !c.sameDoc {
				second = parseRaw(t, rawFlow, "second-project")
			}
			if StableFlowID(first) != StableFlowID(second) || FlowHash(first) != FlowHash(second) {
				t.Fatal("fixture rot: both requests must name the same flow and content")
			}

			var r1, r2 *models.AnalysisReport
			done := make(chan struct{}, 2)
			go func() {
				r1 = a.Analyze(context.Background(), Input{Doc: first, Rules: rules, Tenant: c.tenant1})
				done <- struct{}{}
			}()
			if !gate.await(10 * time.Second) {
				t.Fatal("the first walk did not start")
			}
			go func() {
				r2 = a.Analyze(context.Background(), Input{Doc: second, Rules: rules, Tenant: c.tenant2})
				done <- struct{}{}
			}()
			// The second request must start its own walk while the first is held; one keyed on less than the full
			// key would park on the first's flight instead.
			if !gate.await(5 * time.Second) {
				t.Error("the second request joined the first one's walk")
			}
			gate.open()
			<-done
			<-done

			if n := gate.count(); n != 2 {
				t.Errorf("%d walks, want 2 (one per key)", n)
			}
			if r1 == nil || r2 == nil || r1 == r2 {
				t.Fatalf("reports %p and %p, want two", r1, r2)
			}
			for _, x := range []struct {
				r   *models.AnalysisReport
				doc *models.FlowDocument
			}{{r1, first}, {r2, second}} {
				if x.r.FlowName != x.doc.Name || x.r.FlowID != x.doc.ID {
					t.Errorf("report names flow %q (%s), want the caller's %q (%s)", x.r.FlowName, x.r.FlowID, x.doc.Name, x.doc.ID)
				}
				requireOwnBlocks(t, x.r, x.doc)
			}
		})
	}
}
