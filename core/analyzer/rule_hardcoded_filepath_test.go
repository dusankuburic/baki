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
}
