package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestHardcodedFilePathRule(t *testing.T) {
	rule := &HardcodedFilePathRule{}

	t.Run("Windows absolute path in file property", func(t *testing.T) {
		b := makeBlock("b1", "Read file", models.BlockTypeAction, "File.Read", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"filePath": `C:\Users\admin\Documents\data.csv`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for hardcoded path, got %d", len(got))
		}
	})

	t.Run("Unix absolute path in source property", func(t *testing.T) {
		b := makeBlock("b2", "Copy file", models.BlockTypeAction, "File.Copy", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"sourcePath": `/home/user/data/input.txt`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for hardcoded Unix path, got %d", len(got))
		}
	})

	t.Run("Variable reference does not trigger", func(t *testing.T) {
		b := makeBlock("b3", "Read file", models.BlockTypeAction, "File.Read", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"filePath": `%BaseFolder%\data.csv`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for variable path, got %d", len(got))
		}
	})

	t.Run("Non-path property does not trigger", func(t *testing.T) {
		b := makeBlock("b4", "Set var", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"value": `C:\Users\admin\Documents\data.csv`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for non-path property, got %d", len(got))
		}
	})

	t.Run("relative path does not trigger", func(t *testing.T) {
		b := makeBlock("b5", "Read file", models.BlockTypeAction, "File.Read", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"filePath": `data\input.csv`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for relative path, got %d", len(got))
		}
	})
}

func TestSqlInjectionRiskRule(t *testing.T) {
	rule := &SqlInjectionRiskRule{}

	t.Run("SQL with variable interpolation triggers", func(t *testing.T) {
		b := makeBlock("b1", "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"sql": `SELECT * FROM Users WHERE name = '%UserName%'`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for SQL with variable, got %d", len(got))
		}
	})

	t.Run("SQL with parameterized query does not trigger", func(t *testing.T) {
		b := makeBlock("b2", "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"sql": `SELECT * FROM Users WHERE name = @UserName`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for parameterized SQL, got %d", len(got))
		}
	})

	t.Run("Non-SQL action does not trigger", func(t *testing.T) {
		b := makeBlock("b3", "Set var", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"value": `SELECT * FROM %TableName%`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for non-SQL action, got %d", len(got))
		}
	})

	// Regression: the parser preserves source case for property keys, and PAD
	// emits PascalCase keys ("Sql", "Query"). The rule must match
	// case-insensitively or it never fires on a real parsed flow.
	t.Run("PascalCase Sql key triggers", func(t *testing.T) {
		b := makeBlock("b4", "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"Sql": `SELECT * FROM Users WHERE name = '%UserName%'`,
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for PascalCase Sql key, got %d", len(got))
		}
	})

	// An internal counter chain (SET X TO %Counter% + 1, Counter internal) must
	// NOT escalate to Error — only variables tracing to an untrusted source do.
	// The old varTaintedByUntrusted treated any %ref% in a SET as tainted.
	t.Run("internal counter variable stays at Warning", func(t *testing.T) {
		counter := makeBlock("c1", "Set counter", models.BlockTypeAction, "SetVariable.Set", 0)
		counter.SubflowID = "sf1"
		counter.Properties = map[string]string{"_output": "Counter", "_value": "0"}
		setX := makeBlock("c2", "Set X", models.BlockTypeAction, "SetVariable.Set", 1)
		setX.SubflowID = "sf1"
		setX.Properties = map[string]string{"_output": "X", "_value": "%Counter% + 1"}
		sql := makeBlock("c3", "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 2)
		sql.SubflowID = "sf1"
		sql.Properties = map[string]string{"sql": `SELECT * FROM t WHERE id = %X%`}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*counter, *setX, *sql}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(sql, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].Severity == models.SeverityError {
			t.Errorf("internal counter variable escalated to Error (should stay Warning); finding: %+v", got[0])
		}
	})

	// A variable written by an HTTP action IS untrusted → the SQL finding
	// escalates to Error (confirmed taint path).
	t.Run("HTTP-sourced variable escalates to Error", func(t *testing.T) {
		http := makeBlock("h1", "Invoke service", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
		http.SubflowID = "sf1"
		http.Properties = map[string]string{"_output": "ReqData"}
		sql := makeBlock("h2", "Execute query", models.BlockTypeAction, "Database.ExecuteSql", 1)
		sql.SubflowID = "sf1"
		sql.Properties = map[string]string{"sql": `SELECT * FROM t WHERE name = '%ReqData%'`}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*http, *sql}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(sql, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].Severity != models.SeverityError {
			t.Errorf("HTTP-sourced variable should escalate to Error, got %v; finding: %+v", got[0].Severity, got[0])
		}
	})
}
