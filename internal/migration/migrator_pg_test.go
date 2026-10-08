package migration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/contract"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// TestMigrator_IntoPostgres runs a legacy directory into a real Postgres destination. The other tests use the fake
// as the destination; this pins that Postgres answers the migrator's existence, owner and settings checks the same
// way: legacy versions above 0 still insert, absent owners are adopted, and a rerun skips everything.
func TestMigrator_IntoPostgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })

	operator := "op-" + uuid.NewString()
	if err := pg.CreateUser(ctx, &interfaces.User{ID: operator, Email: operator + "@example.com", Password: "hash", Role: "admin"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	d := contract.NewLegacyDir(t)
	created := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	updated := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, f := range []struct {
		id, owner string
		version   int
	}{{"legacy-1", "ghost", 3}, {"legacy-2", "", 0}, {"legacy-3", operator, 7}} {
		doc := flowDoc(f.id, "Flow "+f.id, f.owner)
		doc.Version = f.version
		doc.Content = json.RawMessage(`{"subflows":[{"name":"Main"}]}`)
		doc.CreatedAt, doc.UpdatedAt = created, updated
		d.Flow(doc)
	}
	d.Settings(&interfaces.AppSettings{Version: 1, Analysis: interfaces.AnalysisSettings{Rules: map[string]interfaces.RuleConfig{
		"deep-nesting": {Enabled: false, Severity: "warning"},
	}}})
	d.Conversation("openai", "legacy-1",
		models.ChatMessage{ID: "m1", Role: "user", Content: "hello", Timestamp: time.Now()},
		models.ChatMessage{ID: "m2", Role: "assistant", Content: "hi", ToolCalls: []models.ToolCallRecord{{Name: "get_block", Ok: true}}},
	)
	src := openSource(t, d)

	res, err := migration.New(src, pg).WithDefaultOwner(operator).Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 3 || res.FlowsAdopted != 2 || !res.SettingsMoved || res.ConversationsMigrated != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, id := range []string{"legacy-1", "legacy-2", "legacy-3"} {
		got, err := pg.LoadFlow(ctx, id)
		if err != nil {
			t.Fatalf("LoadFlow(%s): %v", id, err)
		}
		var content any
		if err := json.Unmarshal(got.Content, &content); err != nil || got.OwnerID != operator || !reflect.DeepEqual(content, map[string]any{"subflows": []any{map[string]any{"name": "Main"}}}) {
			t.Errorf("%s: owner %q content %s", id, got.OwnerID, got.Content)
		}
		if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) || got.Version != 0 {
			t.Errorf("%s: created %v, updated %v, version %d; want the legacy file's %v, %v at version 0",
				id, got.CreatedAt, got.UpdatedAt, got.Version, created, updated)
		}
	}
	settings, err := pg.LoadSettings(ctx)
	if err != nil || settings.Analysis.Rules["deep-nesting"].Severity != "warning" {
		t.Errorf("settings = %+v, %v", settings, err)
	}
	msgs, err := pg.LoadConversation(ctx, "legacy-1", "openai")
	if err != nil || len(msgs) != 2 || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("conversation = %+v, %v", msgs, err)
	}

	res2, err := migration.New(src, pg).WithDefaultOwner(operator).Migrate(ctx)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if res2.FlowsSkipped != 3 || res2.FlowsMigrated != 0 || res2.ConversationsSkipped != 1 || len(res2.Errors) != 0 {
		t.Errorf("rerun result = %+v", res2)
	}
}
