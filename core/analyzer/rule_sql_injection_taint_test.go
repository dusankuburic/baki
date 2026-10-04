package analyzer

import (
	"fmt"
	"testing"
	"time"

	"pad-core/models"
)

// sqlTaintFlow wraps blocks in a single-subflow document, mirroring the
// helpers in rule_hardcoded_filepath_test.go, and returns the built context.
func sqlTaintFlow(blocks ...*models.Block) *RuleContext {
	bs := make([]models.Block, 0, len(blocks))
	for _, b := range blocks {
		b.SubflowID = "sf1"
		bs = append(bs, *b)
	}
	flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: bs}}}
	return buildContext(flow, nil)
}

func sqlTaintSet(id, out, value string, line int) *models.Block {
	b := makeBlock(id, "Set "+out, models.BlockTypeAction, "SetVariable.Set", 0)
	b.LineNumber = line
	b.Properties = map[string]string{"_output": out, "_value": value}
	return b
}

func sqlTaintHTTP(id, out string, line int) *models.Block {
	b := makeBlock(id, "Invoke service", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
	b.LineNumber = line
	b.Properties = map[string]string{"_output": out}
	return b
}

func sqlTaintQuery(id, sql string, line int) *models.Block {
	b := makeBlock(id, "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 0)
	b.LineNumber = line
	b.Properties = map[string]string{"sql": sql}
	return b
}

// checkSqlSeverity runs the rule on the SQL block and returns the severity
// of its single finding.
func checkSqlSeverity(t *testing.T, ctx *RuleContext, sql *models.Block) models.Severity {
	t.Helper()
	got := (&SqlInjectionRiskRule{}).Check(sql, ctx)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	return got[0].Severity
}

// Taint propagates through a SET one hop at a time, to any depth: HTTP → X,
// SET Y TO %X%, SET Z TO %Y% & "x", SQL interpolating %Z% is a confirmed path.
func TestSqlInjectionRiskRule_TaintPropagatesThroughSetChain(t *testing.T) {
	sql := sqlTaintQuery("q", `SELECT * FROM t WHERE name = '%Z%'`, 3)
	ctx := sqlTaintFlow(
		sqlTaintHTTP("h", "X", 0),
		sqlTaintSet("s1", "Y", "%X%", 1),
		sqlTaintSet("s2", "Z", "%Y% & 'suffix'", 2),
		sql,
	)
	if sev := checkSqlSeverity(t, ctx, sql); sev != models.SeverityError {
		t.Errorf("two-hop SET chain from HTTP should escalate to Error, got %v", sev)
	}
	for _, vn := range []string{"X", "Y", "Z"} {
		if !varTaintedByUntrusted(ctx, vn) {
			t.Errorf("varTaintedByUntrusted(%q) = false, want true", vn)
		}
	}
}

// A reference cycle with no untrusted source (SET A TO %B%, SET B TO %A%)
// terminates and stays at the default Warning.
func TestSqlInjectionRiskRule_SetCycleWithoutSourceStaysWarning(t *testing.T) {
	sql := sqlTaintQuery("q", `SELECT * FROM t WHERE id = %A%`, 2)
	ctx := sqlTaintFlow(
		sqlTaintSet("s1", "A", "%B%", 0),
		sqlTaintSet("s2", "B", "%A%", 1),
		sql,
	)
	if sev := checkSqlSeverity(t, ctx, sql); sev == models.SeverityError {
		t.Errorf("source-less SET cycle escalated to Error; want Warning")
	}
	if varTaintedByUntrusted(ctx, "A") || varTaintedByUntrusted(ctx, "B") {
		t.Errorf("source-less cycle marked tainted: A=%v B=%v", ctx.TaintedVars["A"], ctx.TaintedVars["B"])
	}
}

// The same cycle with an HTTP-sourced variable feeding one side taints both
// sides: the cycle guard must not hide a source reachable past the back-edge.
func TestSqlInjectionRiskRule_SetCycleWithSourceEscalates(t *testing.T) {
	sql := sqlTaintQuery("q", `SELECT * FROM t WHERE id = %A%`, 3)
	ctx := sqlTaintFlow(
		sqlTaintHTTP("h", "X", 0),
		sqlTaintSet("s1", "A", "%B%", 1),
		sqlTaintSet("s2", "B", "%A% & %X%", 2),
		sql,
	)
	if sev := checkSqlSeverity(t, ctx, sql); sev != models.SeverityError {
		t.Errorf("cycle fed by HTTP should escalate to Error, got %v", sev)
	}
}

// A context built without buildContext (no TaintedVars) answers false, as the
// old walk did for a nil WritersByVar.
func TestVarTaintedByUntrusted_NilContextIsUntainted(t *testing.T) {
	if varTaintedByUntrusted(nil, "X") || varTaintedByUntrusted(&RuleContext{}, "X") {
		t.Fatal("nil / bare context must report untainted")
	}
}

// Scale regression: taint used to be re-derived per %var% per SQL block by
// walking the variable's transitive writer set with a regex pass per writer.
// A flow where thousands of SETs accumulate into one shared variable (SET X
// TO %X% & %Part_i%) whose HTTP writer is the LAST in document order made that
// walk O(SQL blocks × writers) ≈ 10^7 regex calls — tens of seconds. With taint
// computed once per analysis the whole thing is a few thousand regex calls.
func TestSqlInjectionRiskRule_ScaleManySetsOneSharedVariable(t *testing.T) {
	const n = 3000
	blocks := make([]*models.Block, 0, 2*n+1)
	line := 0
	for i := 0; i < n; i++ {
		blocks = append(blocks, sqlTaintSet(fmt.Sprintf("s%d", i), "X", fmt.Sprintf("%%X%% & %%Part%d%%", i), line))
		line++
	}
	blocks = append(blocks, sqlTaintHTTP("h", "X", line))
	line++
	sqls := make([]*models.Block, 0, n)
	for i := 0; i < n; i++ {
		q := sqlTaintQuery(fmt.Sprintf("q%d", i), fmt.Sprintf(`SELECT * FROM t%d WHERE name = '%%X%%'`, i), line)
		line++
		sqls = append(sqls, q)
		blocks = append(blocks, q)
	}

	start := time.Now()
	ctx := sqlTaintFlow(blocks...)
	rule := &SqlInjectionRiskRule{}
	errors := 0
	for _, q := range sqls {
		got := rule.Check(q, ctx)
		if len(got) == 1 && got[0].Severity == models.SeverityError {
			errors++
		}
	}
	elapsed := time.Since(start)
	if errors != n {
		t.Errorf("want all %d SQL blocks graded Error (X is HTTP-sourced), got %d", n, errors)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("buildContext + %d rule checks took %v, want < 3s (taint re-derived per block?)", n, elapsed)
	}
}
