package analyzer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// The differential oracle for the eight rules converted from per-block Check to
// CheckFlow (Phase 4a): verbatim copies of the legacy Check implementations
// (only fact fields became accessor calls) run per block in walk order on the
// SAME RuleContext as the new CheckFlow, over the corpus, the parser fuzz
// corpus and seeded random subflow graphs. Findings, order and therefore
// fingerprints (a function of the finding and the document) must be identical.

type legacyCircular struct{ Rule }

func (r legacyCircular) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.CircularSubflows()) == 0 {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	if !ctx.CircularSubflows()[subflow.ID] {
		return nil
	}

	// Only emit once per subflow — on the first block of the subflow (the
	// subflow header / first action) to avoid duplicate findings for every
	// block inside the same cyclic subflow.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Circular subflow dependency",
		Description: "Subflow '" + subflow.Name + "' is part of a circular call chain. This will cause infinite recursion at runtime.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Break the cycle by removing one call in the chain or by introducing a termination condition.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

type legacyDuplicateSubflowName struct{ Rule }

func (r legacyDuplicateSubflowName) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.DuplicateSubflowNames()) == 0 {
		return nil
	}
	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	if !ctx.DuplicateSubflowNames()[subflow.Name] {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Duplicate subflow name",
		Description: "Subflow '" + subflow.Name + "' shares its name with at least one other subflow. Only the first is used when CALL resolves by name — the others are silently unreachable.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Rename one of the duplicate subflows so each has a unique name.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

type legacyUncalledSubflow struct{ Rule }

func (r legacyUncalledSubflow) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if ctx.SubflowFanIn() == nil {
		return nil
	}
	if ctx.SubflowFanIn()[block.SubflowID] > 0 {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// The entry-point subflow (first in document order) is the default caller
	// target — never flag it even with fan-in 0.
	if isEntryPoint(subflow, ctx) {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Uncalled subflow",
		Description: "Subflow '" + subflow.Name + "' is never called by any other subflow and is not the entry point. It is dead code at the flow level.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove this subflow if it is no longer needed, or add a call to it from the entry point.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

type legacyEmptySubflow struct{ Rule }

func (r legacyEmptySubflow) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if _, hasParent := ctx.ParentMap[block.ID]; hasParent {
		return nil
	}
	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// Fire on the first top-level block (any type) of the subflow.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}
	if countActionableBlocks(subflow.Blocks) > 0 {
		return nil
	}
	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Empty subflow",
		Description: "Subflow '" + subflow.Name + "' contains no actionable blocks.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove the empty subflow or add actions to it.",
		Metadata:    map[string]interface{}{"subflow": subflow.Name},
	}}
}

type legacyHighCyclomatic struct{ Rule }

func (r legacyHighCyclomatic) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if ctx.SubflowCyclo() == nil {
		return nil
	}
	cyclo := ctx.SubflowCyclo()[block.SubflowID]
	if cyclo <= defaultMaxCyclomatic {
		return nil
	}

	subflow := ctx.SubflowByID[block.SubflowID]
	if subflow == nil {
		return nil
	}
	// Only emit once per subflow — on the first block.
	if len(subflow.Blocks) == 0 || block.ID != subflow.Blocks[0].ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "High cyclomatic complexity",
		Description: "Subflow '" + subflow.Name + "' has a cyclomatic complexity of " + itoa(cyclo) + " (threshold: " + itoa(defaultMaxCyclomatic) + "), indicating excessive branching that is hard to test and maintain.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Split this subflow into smaller subflows, reduce nesting, or extract complex conditions into helper subflows.",
		Metadata:    map[string]interface{}{"complexity": cyclo, "threshold": defaultMaxCyclomatic},
	}}
}

type legacyLargeSubflow struct{ Rule }

func (r legacyLargeSubflow) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.ParentID != "" {
		return nil
	}
	if block.Type == models.BlockTypeComment {
		return nil
	}

	threshold := ruleOption(ctx.Settings, r.ID(), largeSubflowMaxBlocks)

	sf := ctx.SubflowByID[block.SubflowID]
	if sf == nil {
		return nil
	}
	// Fire once per subflow on the first non-comment top-level block (see
	// firstActionableBlockID).
	if legacyFirstActionableBlockID(sf.Blocks) != block.ID {
		return nil
	}
	count := countAllBlocks(sf.Blocks)
	if count > threshold {
		return []models.Finding{{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Large subflow",
			Description: "Subflow '" + sf.Name + "' has " + strconv.Itoa(count) + " blocks (threshold: " + strconv.Itoa(threshold) + ").",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Break the subflow into smaller, focused subflows that each handle a single responsibility.",
		}}
	}

	return nil
}

func legacyFirstActionableBlockID(blocks []models.Block) string {
	for i := range blocks {
		t := blocks[i].Type
		if t != models.BlockTypeComment && t != models.BlockTypeEnd {
			return blocks[i].ID
		}
	}
	return ""
}

type legacySubflowNoErrorHandler struct{ Rule }

func (r legacySubflowNoErrorHandler) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	// Fire once per subflow by triggering only on the first top-level block
	// (no parent entry in ParentMap).
	if _, hasParent := ctx.ParentMap[block.ID]; hasParent {
		return nil
	}
	if block.Type == models.BlockTypeComment {
		return nil
	}

	// Locate the subflow this block belongs to (O(1) via the precomputed index).
	sf := ctx.SubflowByID[block.SubflowID]
	if sf == nil {
		return nil
	}
	// Attach to the first non-comment top-level block so the finding's line /
	// suppression target is an actionable block, not a leading comment.
	if legacyFirstActionableBlockID(sf.Blocks) != block.ID {
		return nil
	}

	if !sfNeedsErrorHandler(sf.Blocks) {
		return nil
	}

	// Check whether any error-handler block exists anywhere in this subflow,
	// including handlers nested inside child blocks.
	if sfHasErrorHandler(sf.Blocks) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Subflow without error handler",
		Description: fmt.Sprintf("Subflow %q has action blocks but no error handler. Unhandled errors will silently terminate the subflow.", sf.Name),
		BlockID:     block.ID,
		SubflowID:   sf.ID,
		Suggestion:  "Add a Try/Catch or On Block Error handler to protect against unexpected failures.",
	}}
}

type legacyParseError struct{ Rule }

func (r legacyParseError) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if len(ctx.Flow.ParseErrors) == 0 {
		return nil
	}
	if len(ctx.Flow.Subflows) == 0 {
		return nil
	}
	firstSF := &ctx.Flow.Subflows[0]
	if len(firstSF.Blocks) == 0 || block.ID != firstSF.Blocks[0].ID {
		return nil
	}

	return buildParseErrorFindings(ctx.Flow.ParseErrors, block.ID, block.SubflowID)
}

// legacyOf returns the pre-4a per-block version of a converted flow rule, under
// the same identity.
func legacyOf(r Rule) (BlockRule, bool) {
	switch r.(type) {
	case *CircularSubflowRule:
		return legacyCircular{r}, true
	case *DuplicateSubflowNameRule:
		return legacyDuplicateSubflowName{r}, true
	case *UncalledSubflowRule:
		return legacyUncalledSubflow{r}, true
	case *EmptySubflowRule:
		return legacyEmptySubflow{r}, true
	case *HighCyclomaticComplexityRule:
		return legacyHighCyclomatic{r}, true
	case *LargeSubflowRule:
		return legacyLargeSubflow{r}, true
	case *SubflowNoErrorHandlerRule:
		return legacySubflowNoErrorHandler{r}, true
	case *ParseErrorRule:
		return legacyParseError{r}, true
	}
	return nil, false
}

// legacyRuleSet is rules with every converted flow rule swapped for its legacy
// per-block version in the same position.
func legacyRuleSet(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r
		if l, ok := legacyOf(r); ok {
			out[i] = l
		}
	}
	return out
}

// legacyDispatch is the pre-4a engine's dispatch of one rule: Check on every
// non-END block in document pre-order.
func legacyDispatch(r BlockRule, ctx *RuleContext) []models.Finding {
	var out []models.Finding
	walkBlocks(ctx.Flow, func(b *models.Block) {
		if b.Type != models.BlockTypeEnd {
			out = append(out, r.Check(b, ctx)...)
		}
	})
	return out
}

type oracleDoc struct {
	name string
	doc  *models.FlowDocument
}

// oracleDocs: the findings-golden corpus, the parser fuzz corpus and seeds,
// and seeded random subflow graphs.
func oracleDocs(t *testing.T, random int) []oracleDoc {
	t.Helper()
	var docs []oracleDoc
	for _, in := range findingsGoldenInputs(t) {
		docs = append(docs, oracleDoc{in.name, in.build(t)})
	}
	texts := []string{
		"#Region \"Main\"\n    COMMENT  test\n#EndRegion",
		"#Region \"A\"\n    LOOP FOREACH X IN %L%\n        IF %X% > 0\n            SET Y TO %X%\n        END\n    END\n#EndRegion",
		"garbage input\n!!!\n\x00\x01",
		"#Region\n" + strings.Repeat("    LOOP\n", 300) + strings.Repeat("    END\n", 300) + "#EndRegion",
		"#Region \"A\"\n#Region \"B\"\n#EndRegion\n#EndRegion",
		"#Region \"Main\"\n    END\n    SET X TO 1\n#EndRegion\n#Region \"Main\"\n    CALL Main\n#EndRegion",
	}
	files, _ := filepath.Glob("../parser/testdata/fuzz/FuzzParser/*")
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- test corpus
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r") // a CRLF checkout
			if strings.HasPrefix(line, "string(") {
				s, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(line, "string("), ")"))
				if err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				texts = append(texts, s)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("parser fuzz corpus not found")
	}
	for i, src := range texts {
		doc, _ := parser.ParseText(src, "fuzz.txt", int64(len(src)))
		if doc != nil {
			docs = append(docs, oracleDoc{fmt.Sprintf("fuzz_%d", i), doc})
		}
	}
	for i, doc := range randomGraphDocs(random, 1) {
		docs = append(docs, oracleDoc{fmt.Sprintf("random_%d", i), doc})
	}
	return docs
}

// randomGraphDocs builds n seeded random documents shaped to hit every branch of
// the converted rules: call cycles and self-calls, calls to missing subflows,
// duplicate and entry-point names (Main not first), empty, comment-only and
// END-led subflows, nested handlers, branchy subflows over the cyclomatic
// threshold, parse errors, and inconsistent hand-built input (a top-level block
// without or with another SubflowID, a stray ParentID, a duplicate subflow ID).
func randomGraphDocs(n int, seed int64) []*models.FlowDocument {
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- deterministic test data
	names := []string{"Main", "main", "A", "B", "C", "Helper", "A", ""}
	var docs []*models.FlowDocument
	for d := 0; d < n; d++ {
		doc := &models.FlowDocument{ID: fmt.Sprintf("rand-%d", d), Name: fmt.Sprintf("Random %d", d)}
		line, ids := 0, 0
		block := func(sfID string, t models.BlockType, raw, name string) models.Block {
			ids++
			line++
			return models.Block{ID: fmt.Sprintf("r%d-b%d", d, ids), SubflowID: sfID, Type: t, RawType: raw, Name: name, LineNumber: line}
		}
		nsf := rng.Intn(7)
		sfNames := make([]string, nsf)
		for i := range sfNames {
			sfNames[i] = names[rng.Intn(len(names))]
		}
		var kids func(sfID, parent string, depth int) []models.Block
		kids = func(sfID, parent string, depth int) []models.Block {
			var out []models.Block
			for k, m := 0, rng.Intn(6); k < m; k++ {
				var b models.Block
				switch rng.Intn(9) {
				case 0:
					b = block(sfID, models.BlockTypeComment, "COMMENT", "note")
				case 1:
					target := "Missing"
					if nsf > 0 {
						target = sfNames[rng.Intn(nsf)]
					}
					b = block(sfID, models.BlockTypeSubflow, "CALL", "Call "+target)
				case 2:
					b = block(sfID, models.BlockTypeErrorHandler, "ON ERROR", "On error")
				case 3:
					b = block(sfID, models.BlockTypeVariable, "SET", "Set X")
					b.Properties = map[string]string{"_output": "X"}
				case 4, 5:
					if depth < 3 {
						b = block(sfID, models.BlockTypeCondition, "IF", "If %X% > 0")
						b.Children = kids(sfID, b.ID, depth+1)
						if rng.Intn(2) == 0 {
							els := block(sfID, models.BlockTypeElse, "ELSE", "Else")
							els.ParentID = b.ID
							b.Children = append(b.Children, els)
						}
						end := block(sfID, models.BlockTypeEnd, "END", "End")
						end.ParentID = b.ID
						b.Children = append(b.Children, end)
						break
					}
					fallthrough
				default:
					b = block(sfID, models.BlockTypeAction, "File.Copy", "Copy file")
				}
				b.ParentID = parent
				out = append(out, b)
			}
			return out
		}
		for i := 0; i < nsf; i++ {
			sfID := fmt.Sprintf("sf-%d", i)
			if i > 0 && rng.Intn(20) == 0 {
				sfID = doc.Subflows[rng.Intn(i)].ID // duplicate subflow ID
			}
			sf := models.Subflow{ID: sfID, Name: sfNames[i]}
			switch rng.Intn(8) {
			case 0: // empty
			case 1: // comment-only
				for k := 0; k <= rng.Intn(3); k++ {
					sf.Blocks = append(sf.Blocks, block(sfID, models.BlockTypeComment, "COMMENT", "only a comment"))
				}
			case 2: // END-led
				sf.Blocks = append(sf.Blocks, block(sfID, models.BlockTypeEnd, "END", "End"))
				sf.Blocks = append(sf.Blocks, kids(sfID, "", 0)...)
			case 3: // over the cyclomatic threshold
				for k := 0; k < 21+rng.Intn(3); k++ {
					sf.Blocks = append(sf.Blocks, block(sfID, models.BlockTypeCondition, "IF", "If"))
				}
			default:
				for k := 0; k < 1+rng.Intn(4); k++ {
					sf.Blocks = append(sf.Blocks, kids(sfID, "", 0)...)
				}
			}
			if len(sf.Blocks) > 0 && rng.Intn(10) == 0 {
				j := rng.Intn(len(sf.Blocks))
				switch rng.Intn(3) {
				case 0:
					sf.Blocks[j].SubflowID = ""
				case 1:
					sf.Blocks[j].SubflowID = fmt.Sprintf("sf-%d", rng.Intn(nsf))
				default:
					sf.Blocks[j].ParentID = "stray"
				}
			}
			doc.Subflows = append(doc.Subflows, sf)
		}
		if rng.Intn(4) == 0 {
			sev := []string{"error", "warning", "info", ""}
			for k := 0; k <= rng.Intn(3); k++ {
				doc.ParseErrors = append(doc.ParseErrors, models.ParseError{Line: k + 1, Message: "bad line", Severity: sev[rng.Intn(len(sev))]})
			}
		}
		doc.RebuildIndexes()
		docs = append(docs, doc)
	}
	return docs
}

func oracleSettings() []*models.AppSettings {
	low := models.DefaultSettings()
	rc := low.Analysis.Rules["large-subflow"]
	rc.Options = map[string]interface{}{"maxBlocks": float64(2)}
	low.Analysis.Rules["large-subflow"] = rc
	return []*models.AppSettings{nil, models.DefaultSettings(), low}
}

func sameFindings(a, b []models.Finding) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestFlowRules_MatchLegacyPerBlockChecks(t *testing.T) {
	fired := map[string]int{}
	for _, d := range oracleDocs(t, 400) {
		for si, settings := range oracleSettings() {
			ctx := buildContext(d.doc, settings)
			for _, r := range AllRules() {
				legacy, ok := legacyOf(r)
				if !ok {
					continue
				}
				got := r.(FlowRule).CheckFlow(ctx)
				sortByDocPosition(ctx, got)
				want := legacyDispatch(legacy, ctx)
				if !sameFindings(got, want) {
					t.Errorf("%s settings#%d %s:\n flow:   %+v\n legacy: %+v", d.name, si, r.ID(), got, want)
				}
				fired[r.ID()] += len(want)
			}
		}
	}
	// Every converted rule fired somewhere, so no comparison was vacuous.
	for _, r := range AllRules() {
		if _, ok := legacyOf(r); ok && fired[r.ID()] == 0 {
			t.Errorf("%s never fired on the oracle inputs", r.ID())
		}
	}
}

// TestFlowRules_EngineMatchesLegacyRuleSet compares whole reports: the engine
// with the converted flow rules against the engine with their legacy per-block
// versions in the same slots (IDs, fingerprints, stats, metrics, profiles and
// the final progress event).
func TestFlowRules_EngineMatchesLegacyRuleSet(t *testing.T) {
	for _, d := range oracleDocs(t, 150) {
		for si, settings := range oracleSettings() {
			type progress struct{ events, cur, total int }
			run := func(rules []Rule) (*models.AnalysisReport, progress) {
				var p progress
				r := RunAnalysisCtxNoProfile(context.Background(), d.doc, rules, settings, func(cur, total int, _ string) {
					p.events++
					p.cur, p.total = cur, total
				})
				r.DurationMs = 0
				return r, p
			}
			got, gp := run(AllRules())
			want, wp := run(legacyRuleSet(AllRules()))
			got.GeneratedAt = want.GeneratedAt
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s settings#%d: reports differ\n flow:   %+v\n legacy: %+v", d.name, si, got.Findings, want.Findings)
			}
			if gp.cur != wp.cur || gp.total != wp.total {
				t.Errorf("%s settings#%d: final progress %d/%d, legacy %d/%d", d.name, si, gp.cur, gp.total, wp.cur, wp.total)
			}
		}
	}
}
