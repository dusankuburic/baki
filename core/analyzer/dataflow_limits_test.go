package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// refAnalyzeDataFlow is the pre-limits AnalyzeDataFlow, kept verbatim as the
// oracle: unbounded edges (O(readers x writers) per variable), taint paths
// and dead data in AllBlocks map order.
func refAnalyzeDataFlow(doc *models.FlowDocument) *models.DataFlowAnalysis {
	result := &models.DataFlowAnalysis{
		Blocks: make(map[string]*models.BlockDataFlow),
	}

	ctx := buildContext(doc, nil)

	for id, block := range ctx.AllBlocks {
		df := &models.BlockDataFlow{
			BlockID:   id,
			SubflowID: block.SubflowID,
			Reads:     block.Variables,
		}

		if block.Properties != nil {
			if out, ok := block.Properties["_output"]; ok && out != "" {
				df.Writes = append(df.Writes, out)
			}
			if v, ok := block.Properties["_var"]; ok && v != "" {
				df.Writes = append(df.Writes, v)
			}
		}

		result.Blocks[id] = df
	}

	for id, df := range result.Blocks {
		for _, readVar := range df.Reads {
			upstream := findWriters(readVar, ctx, id)
			df.UpstreamBlocks = append(df.UpstreamBlocks, upstream...)
		}
	}

	for id, df := range result.Blocks {
		for _, writeVar := range df.Writes {
			downstream := findReadersAfter(writeVar, ctx, id)
			df.DownstreamBlocks = append(df.DownstreamBlocks, downstream...)
		}
	}

	result.TaintPaths = refTaintPaths(doc, ctx)
	result.DeadData = refDeadDataPaths(ctx)

	return result
}

func refTaintPaths(doc *models.FlowDocument, ctx *RuleContext) []models.TaintPath {
	var sources []string
	for _, b := range ctx.AllBlocks {
		if b.Properties != nil {
			if rt, ok := b.Properties["_inputType"]; ok && (rt == "dialog" || rt == "prompt") {
				if out, ok := b.Properties["_output"]; ok && out != "" {
					sources = append(sources, out)
				}
			}
		}
	}

	inputPrefixes := []string{"Input_", "input_", "UserInput_", "userinput_"}
	for _, sf := range doc.Subflows {
		for _, v := range sf.Variables {
			for _, prefix := range inputPrefixes {
				if len(v.Name) > len(prefix) && v.Name[:len(prefix)] == prefix {
					sources = append(sources, v.Name)
				}
			}
		}
	}

	var paths []models.TaintPath
	type sinkEntry struct {
		block *models.Block
		styp  string
	}
	var sinks []sinkEntry
	for _, b := range ctx.AllBlocks {
		if st := findSink(b.RawType); st != "" {
			sinks = append(sinks, sinkEntry{block: b, styp: st})
		}
	}

	for _, source := range sources {
		for _, s := range sinks {
			for _, v := range s.block.Variables {
				if v == source {
					paths = append(paths, models.TaintPath{
						SourceVar: source,
						SinkBlock: s.block.ID,
						SinkType:  s.styp,
						Path:      []string{source, s.block.ID},
					})
					break
				}
			}
		}
	}
	return paths
}

func refDeadDataPaths(ctx *RuleContext) []models.DeadDataPath {
	var dead []models.DeadDataPath

	for _, b := range ctx.AllBlocks {
		outVar := outputVar(b)
		if outVar == "" {
			continue
		}

		if !ctx.UsedVariables[outVar] {
			continue
		}

		siblingKey := ctx.SiblingKey[b.ID]
		termIdx, hasTerm := ctx.TerminatorIndex[siblingKey]
		if !hasTerm {
			continue
		}
		blockIdx, ok := ctx.BlockIndex[b.ID]
		if !ok || blockIdx > termIdx {
			continue
		}

		readers := findReaders(outVar, ctx)
		for _, readerID := range readers {
			readerIdx, ok := ctx.BlockIndex[readerID]
			if !ok {
				continue
			}
			readerSiblingKey := ctx.SiblingKey[readerID]
			readerTermIdx, hasTerm := ctx.TerminatorIndex[readerSiblingKey]
			if hasTerm && readerIdx > readerTermIdx {
				dead = append(dead, models.DeadDataPath{
					Variable:  outVar,
					SetBlock:  b.ID,
					ReadBlock: readerID,
					Reason:    "reader is in dead code (after terminator)",
				})
			}
		}
	}
	return dead
}

// multiset renders a list as sorted strings; the reference emits taint paths
// and dead data in map order, so only their contents are comparable.
func multiset[T any](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%+v", x)
	}
	sort.Strings(out)
	return out
}

// randomDataFlowDoc builds a flow that exercises every data-flow branch:
// a small variable pool (hot variables with many readers and writers),
// repeated reads, blocks writing through _output and _var (sometimes the
// same variable twice), self-reads, equal and non-monotonic line numbers,
// nested children, EXIT terminators with dead code after them, dialog
// sources, File.Write sinks, Input_ subflow variables, END blocks and a
// duplicated block ID.
func randomDataFlowDoc(seed int64, n int) *models.FlowDocument {
	rng := rand.New(rand.NewSource(seed))
	vars := []string{"A", "B", "C", "D", "Input_Name", "UserInput_Path", "Out", "%Expr%"}
	pick := func() string { return vars[rng.Intn(len(vars))] }
	next := 0
	var mk func(depth int, count int) []models.Block
	mk = func(depth, count int) []models.Block {
		blocks := make([]models.Block, 0, count)
		for i := 0; i < count; i++ {
			next++
			id := fmt.Sprintf("b%04d", next)
			if next%97 == 0 {
				id = "dup" // duplicated ID: AllBlocks keeps the last
			}
			b := models.Block{ID: id, Name: id, Type: models.BlockTypeAction, RawType: "Variables.SetVariable",
				LineNumber: rng.Intn(n) + 1, Properties: map[string]string{}}
			switch r := rng.Intn(20); r {
			case 0:
				b.RawType, b.Name = "EXIT", "EXIT"
			case 1:
				b.RawType = "File.WriteText"
			case 2:
				b.RawType = "Display.InputDialog"
				b.Properties["_inputType"] = "dialog"
				b.Properties["_output"] = pick()
			case 3:
				b.Type = models.BlockTypeEnd
			}
			if rng.Intn(3) == 0 {
				b.Properties["_output"] = pick()
			}
			if rng.Intn(5) == 0 {
				b.Properties["_var"] = pick()
			}
			for k := rng.Intn(4); k > 0; k-- {
				v := pick()
				b.Variables = append(b.Variables, v)
				b.Properties["Value"] = "%" + v + "%" // marks v used
			}
			if depth < 2 && rng.Intn(6) == 0 {
				b.Type = models.BlockTypeLoop
				b.Children = mk(depth+1, 1+rng.Intn(6))
			}
			blocks = append(blocks, b)
		}
		return blocks
	}
	doc := &models.FlowDocument{ID: fmt.Sprintf("rand-%d", seed)}
	for s := 0; s < 3; s++ {
		sf := models.Subflow{ID: fmt.Sprintf("sf%d", s), Name: fmt.Sprintf("SF%d", s), Blocks: mk(0, n/3)}
		sf.Variables = []models.VariableDecl{{Name: "Input_Name"}, {Name: "Other"}}
		for i := range sf.Blocks {
			sf.Blocks[i].SubflowID = sf.ID
		}
		doc.Subflows = append(doc.Subflows, sf)
	}
	return doc
}

func sampleDocs(t *testing.T) []*models.FlowDocument {
	t.Helper()
	files, _ := filepath.Glob("../parser/testdata/samples/*.txt")
	more, _ := filepath.Glob("../parser/testdata/samples/*/*.txt")
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatal("fixture rot: no sample flows")
	}
	var docs []*models.FlowDocument
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := parser.ParseText(string(b), filepath.Base(f), int64(len(b)))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		doc.Name = f
		docs = append(docs, doc)
	}
	return docs
}

// TestAnalyzeDataFlow_MatchesUnboundedReference: on the sample flows and on
// random flows within the default limits the bounded analysis returns
// exactly what the old unbounded one did — every block's lists in the same
// order, the same taint paths and dead data — and reports nothing truncated.
func TestAnalyzeDataFlow_MatchesUnboundedReference(t *testing.T) {
	docs := sampleDocs(t)
	for seed := int64(1); seed <= 40; seed++ {
		docs = append(docs, randomDataFlowDoc(seed, 60+int(seed)*5))
	}
	docs = append(docs, hotVariableDoc(4, 7), hotVariableDoc(9, 3))
	sawTaint, sawDead, sawUp, sawDown := false, false, false, false
	for _, doc := range docs {
		want := refAnalyzeDataFlow(doc)
		res, err := AnalyzeDataFlowCtx(context.Background(), doc, DefaultDataFlowLimits)
		if err != nil {
			t.Fatal(err)
		}
		if res.Truncated {
			t.Fatalf("%s: truncated under the default limits", doc.Name)
		}
		if !reflect.DeepEqual(res.Blocks, want.Blocks) {
			for id, w := range want.Blocks {
				if !reflect.DeepEqual(res.Blocks[id], w) {
					t.Errorf("%s: block %s:\n got %+v\nwant %+v", doc.Name, id, res.Blocks[id], w)
					break
				}
			}
			t.Fatalf("%s: blocks differ from the reference", doc.Name)
		}
		if got, w := multiset(res.TaintPaths), multiset(want.TaintPaths); !reflect.DeepEqual(got, w) {
			t.Fatalf("%s: taint paths\n got %v\nwant %v", doc.Name, got, w)
		}
		if got, w := multiset(res.DeadData), multiset(want.DeadData); !reflect.DeepEqual(got, w) {
			t.Fatalf("%s: dead data\n got %v\nwant %v", doc.Name, got, w)
		}
		if (res.TaintPaths == nil) != (want.TaintPaths == nil) || (res.DeadData == nil) != (want.DeadData == nil) {
			t.Fatalf("%s: nil-ness of taint paths / dead data differs (JSON null vs [])", doc.Name)
		}
		sawTaint = sawTaint || len(want.TaintPaths) > 0
		sawDead = sawDead || len(want.DeadData) > 0
		for _, b := range want.Blocks {
			sawUp = sawUp || len(b.UpstreamBlocks) > 0
			sawDown = sawDown || len(b.DownstreamBlocks) > 0
		}
		// The wrapper is the same analysis.
		if legacy := AnalyzeDataFlow(doc); !reflect.DeepEqual(legacy.Blocks, res.Blocks) {
			t.Fatalf("%s: AnalyzeDataFlow differs from AnalyzeDataFlowCtx", doc.Name)
		}
	}
	if !sawTaint || !sawDead || !sawUp || !sawDown {
		t.Fatalf("fixture rot: corpus covers taint=%v dead=%v upstream=%v downstream=%v", sawTaint, sawDead, sawUp, sawDown)
	}
}

// hotVariableDoc is the shape that made the response quadratic: one
// flow-global variable written by `writers` blocks and read by `readers`
// blocks, interleaved, all in one sibling scope that ends in EXIT with the
// readers repeated after it (dead), dialog sources and File.Write sinks.
func hotVariableDoc(writers, readers int) *models.FlowDocument {
	var blocks []models.Block
	line := 0
	add := func(b models.Block) {
		line++
		b.LineNumber = line
		b.SubflowID = "sf"
		blocks = append(blocks, b)
	}
	for i := 0; i < writers || i < readers; i++ {
		if i < writers {
			add(models.Block{ID: fmt.Sprintf("w%05d", i), Type: models.BlockTypeAction, RawType: "Display.InputDialog",
				Properties: map[string]string{"_output": "Hot", "_inputType": "dialog"}})
		}
		if i < readers {
			add(models.Block{ID: fmt.Sprintf("r%05d", i), Type: models.BlockTypeAction, RawType: "File.WriteText",
				Variables: []string{"Hot"}, Properties: map[string]string{"Text": "%Hot%"}})
		}
	}
	add(models.Block{ID: "exit", Name: "EXIT", Type: models.BlockTypeAction, RawType: "EXIT"})
	for i := 0; i < readers; i++ {
		add(models.Block{ID: fmt.Sprintf("x%05d", i), Type: models.BlockTypeAction, RawType: "File.WriteText",
			Variables: []string{"Hot"}})
	}
	return &models.FlowDocument{ID: "hot", Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: blocks}}}
}

// TestAnalyzeDataFlowCtx_BoundsHotVariable: 2,000 writers x 4,000 readers of
// one variable was 8M upstream and ~6M downstream edges, 8M taint paths and
// 4M dead-data pairs. Every list must stay within its limit and the result
// must say it was truncated.
func TestAnalyzeDataFlowCtx_BoundsHotVariable(t *testing.T) {
	doc := hotVariableDoc(2000, 2000)
	limits := DataFlowLimits{MaxEdgesPerVariable: 50, MaxEdges: 20_000, MaxTaintPaths: 1_000, MaxDeadData: 1_000}
	res, err := AnalyzeDataFlowCtx(context.Background(), doc, limits)
	if err != nil {
		t.Fatal(err)
	}
	up, down := 0, 0
	for id, b := range res.Blocks {
		if len(b.UpstreamBlocks) > limits.MaxEdgesPerVariable*len(b.Reads) {
			t.Fatalf("block %s lists %d upstream blocks for %d reads", id, len(b.UpstreamBlocks), len(b.Reads))
		}
		if len(b.DownstreamBlocks) > limits.MaxEdgesPerVariable*len(b.Writes) {
			t.Fatalf("block %s lists %d downstream blocks for %d writes", id, len(b.DownstreamBlocks), len(b.Writes))
		}
		up += len(b.UpstreamBlocks)
		down += len(b.DownstreamBlocks)
	}
	if up > limits.MaxEdges || down > limits.MaxEdges {
		t.Errorf("edges: %d upstream, %d downstream, limit %d each", up, down, limits.MaxEdges)
	}
	if up < limits.MaxEdges || down < limits.MaxEdges {
		t.Errorf("edges: %d upstream, %d downstream; the budget should be used up (%d)", up, down, limits.MaxEdges)
	}
	if len(res.TaintPaths) != limits.MaxTaintPaths || len(res.DeadData) != limits.MaxDeadData {
		t.Errorf("taint paths %d (limit %d), dead data %d (limit %d)", len(res.TaintPaths), limits.MaxTaintPaths, len(res.DeadData), limits.MaxDeadData)
	}
	if !res.Truncated || !res.EdgesTruncated || !res.TaintPathsTruncated || !res.DeadDataTruncated {
		t.Errorf("truncation flags: %+v", res)
	}

	// The default limits bound the JSON body too: one hot variable used to be
	// O(writers x readers) block IDs.
	def, err := AnalyzeDataFlowCtx(context.Background(), doc, DefaultDataFlowLimits)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 16<<20 {
		t.Errorf("default-limit response is %d MB", len(body)>>20)
	}
}

// TestAnalyzeDataFlowCtx_PerVariableOrder: when a list is cut the entries
// kept are still listed in index order, and the edges that fit are the
// nearest later readers.
func TestAnalyzeDataFlowCtx_PerVariableOrder(t *testing.T) {
	doc := hotVariableDoc(3, 10)
	res, err := AnalyzeDataFlowCtx(context.Background(), doc, DataFlowLimits{MaxEdgesPerVariable: 2})
	if err != nil {
		t.Fatal(err)
	}
	// w00000 is on line 1; the readers after it are r00000 (line 2), r00001...
	if got := res.Blocks["w00000"].DownstreamBlocks; !reflect.DeepEqual(got, []string{"r00000", "r00001"}) {
		t.Errorf("w00000 downstream = %v", got)
	}
	if got := res.Blocks["r00005"].UpstreamBlocks; !reflect.DeepEqual(got, []string{"w00000", "w00001"}) {
		t.Errorf("r00005 upstream = %v", got)
	}
	if !res.EdgesTruncated {
		t.Error("EdgesTruncated not set")
	}
}

// TestComputeDeadDataPaths_ReadersOncePerVariable: the dead readers of a
// variable are computed once, not once per writer. 2,000 live writers of
// one variable read only by live blocks produced no output but made the old
// loop call findReaders (one 2,000-entry slice) per writer.
func TestComputeDeadDataPaths_ReadersOncePerVariable(t *testing.T) {
	doc := hotVariableDoc(2000, 2000)
	// Drop the dead readers after EXIT so there is no output at all.
	sf := &doc.Subflows[0]
	for i, b := range sf.Blocks {
		if b.ID == "exit" {
			sf.Blocks = sf.Blocks[:i+1]
			break
		}
	}
	rctx := buildContext(doc, nil)
	ids := make([]string, 0, len(rctx.AllBlocks))
	for id := range rctx.AllBlocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var dead []models.DeadDataPath
	allocs := testing.AllocsPerRun(3, func() { dead, _ = computeDeadDataPaths(rctx, ids, 1<<30) })
	if len(dead) != 0 {
		t.Fatalf("fixture rot: %d dead paths", len(dead))
	}
	if allocs > 20 {
		t.Errorf("computeDeadDataPaths: %.0f allocations for one variable with 2000 writers, want <= 20", allocs)
	}
}

// TestAnalyzeDataFlowCtx_Cancelled stops on a done context.
func TestAnalyzeDataFlowCtx_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := AnalyzeDataFlowCtx(ctx, hotVariableDoc(10, 10), DefaultDataFlowLimits)
	if !errors.Is(err, context.Canceled) || res != nil {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", res, err)
	}
}

// TestDataFlowResult_JSONShape: the response keeps every old field at the
// top level and adds the truncation report.
func TestDataFlowResult_JSONShape(t *testing.T) {
	res, err := AnalyzeDataFlowCtx(context.Background(), hotVariableDoc(2, 2), DefaultDataFlowLimits)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"blocks", "taintPaths", "deadData", "truncated", "limits"} {
		if _, ok := m[k]; !ok {
			t.Errorf("response has no %q: %s", k, body)
		}
	}
	if string(m["truncated"]) != "false" {
		t.Errorf("truncated = %s", m["truncated"])
	}
}
