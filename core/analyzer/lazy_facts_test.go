package analyzer

import (
	"cmp"
	"context"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

// factGroups lists every lazy fact group of RuleContext with the accessor that
// forces it; TestFactGroups_Complete keeps it in step with the struct.
var factGroups = []struct {
	name  string
	built func(*RuleContext) bool
	force func(*RuleContext)
}{
	{"usedVariables", func(c *RuleContext) bool { return c.usedVariables.built }, func(c *RuleContext) { c.UsedVariables() }},
	{"signatures", func(c *RuleContext) bool { return c.signatures.built }, func(c *RuleContext) { c.signatures.get(c, buildSignatures) }},
	{"hasErrorHandler", func(c *RuleContext) bool { return c.hasErrorHandler.built }, func(c *RuleContext) { c.HasErrorHandler() }},
	{"insideRetryLoop", func(c *RuleContext) bool { return c.insideRetryLoop.built }, func(c *RuleContext) { c.InsideRetryLoop() }},
	{"allReadersDead", func(c *RuleContext) bool { return c.allReadersDead.built }, func(c *RuleContext) { c.AllReadersDead() }},
	{"labels", func(c *RuleContext) bool { return c.labels.built }, func(c *RuleContext) { c.LabelByName() }},
	{"closedResources", func(c *RuleContext) bool { return c.closedResources.built }, func(c *RuleContext) { c.ClosedResourceVars() }},
	{"taintedVars", func(c *RuleContext) bool { return c.taintedVars.built }, func(c *RuleContext) { c.TaintedVars() }},
	{"graph", func(c *RuleContext) bool { return c.graph.built }, func(c *RuleContext) { c.subflowGraph() }},
	{"docPos", func(c *RuleContext) bool { return c.docPos.built }, func(c *RuleContext) { c.docPosition() }},
}

func builtFacts(ctx *RuleContext) []string {
	var out []string
	for _, g := range factGroups {
		if g.built(ctx) {
			out = append(out, g.name)
		}
	}
	return out
}

func forceAllFacts(ctx *RuleContext) {
	for _, g := range factGroups {
		g.force(ctx)
	}
}

func TestFactGroups_Complete(t *testing.T) {
	var lazyFields []string
	rt := reflect.TypeOf(RuleContext{})
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); strings.HasPrefix(f.Type.Name(), "lazy[") {
			lazyFields = append(lazyFields, f.Name)
		}
	}
	var listed []string
	for _, g := range factGroups {
		listed = append(listed, g.name)
	}
	sort.Strings(lazyFields)
	sort.Strings(listed)
	if !slices.Equal(lazyFields, listed) {
		t.Fatalf("RuleContext lazy fields %v, factGroups %v", lazyFields, listed)
	}
}

// eagerFacts is every rule-only fact built the pre-4a way (eagerly, in one
// goroutine, by buildContext's old code), the oracle for the lazy accessors.
type eagerFacts struct {
	UsedVariables   map[string]bool
	Signatures      map[string]string
	HasErrorHandler map[string]bool
	InsideRetryLoop map[string]bool
	AllReadersDead  map[string]bool
	LabelByName     map[string]*models.Block
	LabelNameCount  map[string]int
	ClosedResources map[string]map[string]bool
	TaintedVars     map[string]bool
	Circular        []string
	CircularSet     map[string]bool
	SubflowMetrics  []models.SubflowMetrics
	Cyclo           map[string]int
	FanIn           map[string]int
	DuplicateNames  map[string]bool
	DocPos          map[string]int
}

func buildEagerFacts(ctx *RuleContext) eagerFacts {
	flow := ctx.Flow
	var e eagerFacts

	// UsedVariables: scanned per sibling group inside collectBlocks.
	e.UsedVariables = map[string]bool{}
	scan := func(val string) {
		if !strings.Contains(val, "%") {
			return
		}
		for _, m := range variableRefRegex.FindAllStringSubmatch(val, -1) {
			if len(m) > 1 {
				e.UsedVariables[strings.TrimSpace(m[1])] = true
			}
		}
	}
	var collect func(blocks []models.Block)
	collect = func(blocks []models.Block) {
		for i := range blocks {
			if blocks[i].Type == models.BlockTypeEnd {
				continue
			}
			for _, val := range blocks[i].Properties {
				scan(val)
			}
			for _, v := range blocks[i].Variables {
				scan(v)
			}
		}
		for i := range blocks {
			if len(blocks[i].Children) > 0 {
				collect(blocks[i].Children)
			}
		}
	}
	for i := range flow.Subflows {
		collect(flow.Subflows[i].Blocks)
	}

	e.Signatures = map[string]string{}
	for _, b := range ctx.AllBlocks {
		e.Signatures[b.ID] = blockSignature(b)
	}

	e.HasErrorHandler = map[string]bool{}
	var hasHandler func(id string) bool
	hasHandler = func(id string) bool {
		if v, ok := e.HasErrorHandler[id]; ok {
			return v
		}
		e.HasErrorHandler[id] = false
		pid, ok := ctx.ParentMap[id]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		direct := parent.Type == models.BlockTypeErrorHandler || ctx.GroupHasHandler[pid]
		result := direct || hasHandler(pid)
		e.HasErrorHandler[id] = result
		return result
	}
	e.InsideRetryLoop = map[string]bool{}
	retryParent := map[string]bool{}
	var inRetry func(id string) bool
	inRetry = func(id string) bool {
		if v, ok := e.InsideRetryLoop[id]; ok {
			return v
		}
		e.InsideRetryLoop[id] = false
		pid, ok := ctx.ParentMap[id]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		isRetry, ok := retryParent[pid]
		if !ok {
			isRetry = parentIsRetryLoop(parent)
			retryParent[pid] = isRetry
		}
		result := isRetry || inRetry(pid)
		e.InsideRetryLoop[id] = result
		return result
	}
	for id := range ctx.AllBlocks {
		hasHandler(id)
		inRetry(id)
	}

	if len(ctx.TerminatorIndex) > 0 {
		e.AllReadersDead = buildAllReadersDead(ctx)
	}

	e.LabelByName = map[string]*models.Block{}
	e.LabelNameCount = map[string]int{}
	walkBlocks(flow, func(b *models.Block) {
		if b.RawType == "LABEL" {
			key := strings.ToLower(b.Name)
			if _, ok := e.LabelByName[key]; !ok {
				e.LabelByName[key] = b
			}
			e.LabelNameCount[key]++
		}
	})
	e.ClosedResources = buildClosedResourceVars(ctx)
	e.TaintedVars = buildTaintedVars(ctx)

	callGraph := buildCallGraph(flow)
	e.Circular = detectCircularDeps(flow, callGraph)
	e.CircularSet = map[string]bool{}
	for _, id := range e.Circular {
		e.CircularSet[id] = true
	}
	fanInByID := map[string]int{}
	for src, targets := range callGraph {
		for tgt := range targets {
			if tgt != src {
				fanInByID[tgt]++
			}
		}
	}
	e.SubflowMetrics = make([]models.SubflowMetrics, 0, len(flow.Subflows))
	e.Cyclo, e.FanIn = map[string]int{}, map[string]int{}
	e.DuplicateNames = map[string]bool{}
	nameCount := map[string]int{}
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		m := computeSubflowMetrics(sf, callGraph, fanInByID)
		e.SubflowMetrics = append(e.SubflowMetrics, m)
		e.Cyclo[sf.ID] = m.CyclomaticComplexity
		e.FanIn[sf.ID] = m.FanIn
		nameCount[sf.Name]++
	}
	for name, n := range nameCount {
		if n > 1 {
			e.DuplicateNames[name] = true
		}
	}

	e.DocPos = map[string]int{}
	n := 0
	walkBlocks(flow, func(b *models.Block) {
		if _, dup := e.DocPos[b.ID]; !dup {
			e.DocPos[b.ID] = n
		}
		n++
	})
	return e
}

// lazyAsEager reads every fact through the accessors (forcing the groups no
// rule touched) in the eagerFacts shape.
func lazyAsEager(ctx *RuleContext) eagerFacts {
	forceAllFacts(ctx)
	g := ctx.subflowGraph()
	return eagerFacts{
		UsedVariables:   ctx.UsedVariables(),
		Signatures:      ctx.signatures.v,
		HasErrorHandler: ctx.HasErrorHandler(),
		InsideRetryLoop: ctx.InsideRetryLoop(),
		AllReadersDead:  ctx.AllReadersDead(),
		LabelByName:     ctx.LabelByName(),
		LabelNameCount:  ctx.LabelNameCount(),
		ClosedResources: ctx.ClosedResourceVars(),
		TaintedVars:     ctx.TaintedVars(),
		Circular:        g.circular,
		CircularSet:     ctx.CircularSubflows(),
		SubflowMetrics:  g.metrics,
		Cyclo:           ctx.SubflowCyclo(),
		FanIn:           ctx.SubflowFanIn(),
		DuplicateNames:  ctx.DuplicateSubflowNames(),
		DocPos:          ctx.docPosition(),
	}
}

func diffFacts(t *testing.T, name string, got, want eagerFacts) {
	t.Helper()
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < gv.NumField(); i++ {
		g, w := gv.Field(i), wv.Field(i)
		// An empty and a nil map or slice are the same fact.
		if g.Len() == 0 && w.Len() == 0 {
			continue
		}
		if !reflect.DeepEqual(g.Interface(), w.Interface()) {
			t.Errorf("%s: fact %s differs:\n lazy:  %v\n eager: %v", name, gv.Type().Field(i).Name, g.Interface(), w.Interface())
		}
	}
}

// TestLazyFacts_MatchEagerBuild: for every corpus input and the random graph
// documents, the facts a full analysis built on first touch (from the shards,
// for the inputs large enough to shard) plus the ones forced afterwards equal
// the eager pre-4a build, field by field.
func TestLazyFacts_MatchEagerBuild(t *testing.T) {
	var docs []struct {
		name string
		doc  *models.FlowDocument
	}
	for _, in := range findingsGoldenInputs(t) {
		docs = append(docs, struct {
			name string
			doc  *models.FlowDocument
		}{in.name, in.build(t)})
	}
	for i, doc := range randomGraphDocs(60, 7) {
		docs = append(docs, struct {
			name string
			doc  *models.FlowDocument
		}{"random_" + itoaTest(i), doc})
	}
	for _, d := range docs {
		ctx := buildContext(d.doc, models.DefaultSettings())
		if _, complete := analyzeContext(context.Background(), ctx, time.Now(), AllRules(), models.DefaultSettings(), nil, false); !complete {
			t.Fatalf("%s: walk incomplete", d.name)
		}
		diffFacts(t, d.name, lazyAsEager(ctx), buildEagerFacts(buildContext(d.doc, nil)))
	}
}

// ruleFacts is which lazy fact groups each built-in reads; a single-rule walk
// may build only these, plus the graph (the metrics phase). docPos is never
// built: the built-in flow rules anchor on top-level blocks, whose order
// inTopLevelOrder reads off the structural index.
var ruleFacts = map[string][]string{
	"unused-variable":             {"usedVariables"},
	"duplicate-action":            {"signatures"},
	"file-op-no-error-handler":    {"hasErrorHandler"},
	"unhandled-error":             {"hasErrorHandler"},
	"missing-retry":               {"hasErrorHandler", "insideRetryLoop"},
	"dead-data":                   {"allReadersDead"},
	"duplicate-label":             {"labels"},
	"goto-antipattern":            {"labels"},
	"resource-leak":               {"closedResources"},
	"sql-injection-risk":          {"taintedVars"},
	"circular-subflow-dependency": {"graph"},
	"duplicate-subflow-name":      {"graph"},
	"high-cyclomatic-complexity":  {"graph"},
	"uncalled-subflow":            {"graph"},
}

// TestLazyFacts_NotBuiltWhenNoRuleNeedsThem runs each built-in alone (and the
// custom-rule tester's single custom rule) over the corpus and checks that the
// walk built no fact the rule does not read.
func TestLazyFacts_NotBuiltWhenNoRuleNeedsThem(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus x rules")
	}
	custom, err := NewCustomRule(CustomRuleConfig{ID: "custom-box", Name: "Box", Severity: "warning", RawTypeMatch: "^Display\\."})
	if err != nil {
		t.Fatal(err)
	}
	rules := append(AllRules(), custom)
	seen := map[string]bool{}
	for _, in := range findingsGoldenInputs(t) {
		doc := in.build(t)
		for _, r := range rules {
			ctx := buildContext(doc, nil)
			analyzeContext(context.Background(), ctx, time.Now(), []Rule{r}, nil, nil, false)
			allowed := append([]string{"graph"}, ruleFacts[r.ID()]...)
			for _, f := range builtFacts(ctx) {
				seen[r.ID()+"/"+f] = true
				if !slices.Contains(allowed, f) {
					t.Errorf("%s: a walk of %s alone built fact %s", in.name, r.ID(), f)
				}
			}
		}
	}
	// The table is not stale: every listed fact is read on some input.
	for id, facts := range ruleFacts {
		for _, f := range facts {
			if !seen[id+"/"+f] {
				t.Errorf("ruleFacts says %s reads %s, but no corpus walk of it built that fact", id, f)
			}
		}
	}
}

// TestSortByDocPosition_BuiltInsBuildNoDocPosition: whole-rule-set walks over
// the corpus and random graphs (subflow rules firing in several subflows)
// order every flow rule's findings without the docPosition map.
func TestSortByDocPosition_BuiltInsBuildNoDocPosition(t *testing.T) {
	multi := 0
	for _, d := range oracleDocs(t, 150) {
		for si, settings := range oracleSettings() {
			ctx := buildContext(d.doc, settings)
			report, _ := analyzeContext(context.Background(), ctx, time.Now(), AllRules(), settings, nil, false)
			if ctx.docPos.built {
				t.Errorf("%s settings#%d: the walk built the docPosition map", d.name, si)
			}
			anchors := map[string]map[string]bool{}
			for _, f := range report.Findings {
				if kindOf(mustLookupRule(t, f.RuleID)) == kindFlow && f.BlockID != "" {
					if anchors[f.RuleID] == nil {
						anchors[f.RuleID] = map[string]bool{}
					}
					anchors[f.RuleID][f.BlockID] = true
				}
			}
			for _, set := range anchors {
				if len(set) > 1 {
					multi++
				}
			}
		}
	}
	// Not vacuous: flow rules fired on two or more blocks of one document.
	if multi == 0 {
		t.Fatal("no flow rule fired on two or more blocks")
	}
}

func mustLookupRule(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range AllRules() {
		if r.ID() == id {
			return r
		}
	}
	t.Fatalf("no built-in %s", id)
	return nil
}

// TestInTopLevelOrder_ImpliesDocumentOrder: the pre-check that spares the
// docPosition map never calls a list sorted that the map would reorder.
// Anchors are random top-level, nested and END blocks plus block-less
// findings, on random graphs (some with a duplicated block ID) and the corpus.
func TestInTopLevelOrder_ImpliesDocumentOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(5)) // #nosec G404 -- deterministic test data
	docs := randomGraphDocs(300, 11)
	for i, doc := range docs {
		if i%5 != 0 {
			continue
		}
		var blocks []*models.Block
		walkBlocks(doc, func(b *models.Block) {
			if b.Type != models.BlockTypeEnd {
				blocks = append(blocks, b)
			}
		})
		if len(blocks) > 1 {
			blocks[len(blocks)-1].ID = blocks[0].ID
			doc.RebuildIndexes()
		}
	}
	for _, in := range findingsGoldenInputs(t) {
		docs = append(docs, in.build(t))
	}
	decided, dupDocs := 0, 0
	for di, doc := range docs {
		ctx := buildContext(doc, nil)
		if len(ctx.AllBlocks) != ctx.totalBlocks {
			dupDocs++
		}
		var all, top []string
		walkBlocks(doc, func(b *models.Block) { all = append(all, b.ID) })
		for _, sf := range doc.Subflows {
			for _, b := range sf.Blocks {
				top = append(top, b.ID)
			}
		}
		if len(all) == 0 {
			continue
		}
		pos := buildContext(doc, nil).docPosition()
		key := func(f models.Finding) int {
			if p, ok := pos[f.BlockID]; ok && f.BlockID != "" {
				return p
			}
			return math.MaxInt
		}
		byPos := func(a, b models.Finding) int { return cmp.Compare(key(a), key(b)) }
		for trial := 0; trial < 40; trial++ {
			fs := make([]models.Finding, 2+rng.Intn(5))
			for i := range fs {
				switch r := rng.Intn(10); {
				case r == 0:
				case r < 3:
					fs[i].BlockID = all[rng.Intn(len(all))]
				default:
					fs[i].BlockID = top[rng.Intn(len(top))]
				}
			}
			if trial%2 == 0 {
				slices.SortStableFunc(fs, byPos)
			}
			if inTopLevelOrder(ctx, fs) {
				decided++
				if !slices.IsSortedFunc(fs, byPos) {
					t.Errorf("doc %d: inTopLevelOrder says %v is in order", di, fs)
				}
			}
		}
		if ctx.docPos.built {
			t.Errorf("doc %d: inTopLevelOrder built the docPosition map", di)
		}
	}
	if decided == 0 || dupDocs == 0 {
		t.Fatalf("vacuous: %d decided lists, %d documents with a duplicate block ID", decided, dupDocs)
	}
}

// TestLazyFacts_ConcurrentFirstTouch: with the walk sharded, several shards
// race to the first read of a fact; run under -race. Every walk must agree
// with a sequential, eagerly built reference.
func TestLazyFacts_ConcurrentFirstTouch(t *testing.T) {
	doc := goldenIndexed(buildLargeFlow(3000))
	settings := models.DefaultSettings()
	want := RunAnalysis(doc, AllRules(), settings, nil)
	for i := 0; i < 4; i++ {
		ctx := buildContext(doc, settings)
		got, complete := analyzeContext(context.Background(), ctx, time.Now(), AllRules(), settings, nil, false)
		if !complete || !reflect.DeepEqual(got.Findings, want.Findings) {
			t.Fatalf("run %d: findings differ from the reference run", i)
		}
		diffFacts(t, "large_3000", lazyAsEager(ctx), buildEagerFacts(buildContext(doc, nil)))
	}
}
