package database

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
)

// Settings stored per section with a version (ADR 0012), dual-written with the legacy blob for one release.

func newSettingsTestUser(t *testing.T, b *PostgresStorageBackend) string {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("settings-sections-%d", time.Now().UnixNano())
	if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "h"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
	return id
}

func sampleSettings() *interfaces.AppSettings {
	return &interfaces.AppSettings{
		Version:    1,
		General:    interfaces.GeneralSettings{FirstRunCompleted: true},
		Appearance: interfaces.AppearanceSettings{Theme: "nord", Density: "compact", ChatTextSize: "lg"},
		Layout:     interfaces.LayoutSettings{SidebarWidth: 300, LastViewMode: "graph"},
		AI: interfaces.AISettings{
			ActiveProvider:     "openai",
			Providers:          map[string]interfaces.AIProviderConfig{"openai": {Enabled: true, DefaultModel: "gpt", Temperature: 0.2}},
			SystemPromptSuffix: "Be brief.",
			Prompts:            interfaces.AIPromptsConfig{Flow: []string{"Summarize"}},
		},
		Parser: interfaces.ParserSettings{MaxFileSizeMB: 20},
		Analysis: interfaces.AnalysisSettings{
			Rules:             map[string]interfaces.RuleConfig{"dead-code": {Enabled: false, Severity: "info", Options: map[string]any{"n": 3.0}}},
			AutoAnalyzeOnOpen: true,
		},
	}
}

type sectionRowState struct {
	version int
	data    json.RawMessage
	digest  *string
}

func readSectionRows(t *testing.T, b *PostgresStorageBackend, scope, id string) map[string]sectionRowState {
	t.Helper()
	rows, err := b.db.QueryContext(context.Background(),
		`SELECT section, version, data, legacy_digest FROM settings_sections WHERE scope = $1 AND scope_id = $2`, scope, id)
	if err != nil {
		t.Fatalf("read section rows: %v", err)
	}
	defer rows.Close()
	out := map[string]sectionRowState{}
	for rows.Next() {
		var key string
		var r sectionRowState
		var data []byte
		if err := rows.Scan(&key, &r.version, &data, &r.digest); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.data = data
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func mustExec(t *testing.T, b *PostgresStorageBackend, query string, args ...any) {
	t.Helper()
	if _, err := b.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func jsonField(t *testing.T, raw json.RawMessage, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m[key]
}

// TestPostgres_SettingsSections_WritesBothShapes: a write stores every section at its version and the legacy blob
// the release before reads, and that release, decoding the blob as it always did, sees exactly what was written.
func TestPostgres_SettingsSections_WritesBothShapes(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	org := "settings-sections-org-" + user
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Sections", OwnerID: user}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), org) })

	want := sampleSettings()
	for _, tc := range []struct {
		scope, id, table, keyColumn string
		key                         any
		save                        func() error
		load                        func() (*interfaces.AppSettings, error)
	}{
		{"user", user, "user_settings", "user_id", user,
			func() error { return b.SaveUserSettings(ctx, user, want) },
			func() (*interfaces.AppSettings, error) { return b.LoadUserSettings(ctx, user) }},
		{"org", org, "org_settings", "org_id", org,
			func() error { return b.SaveOrgSettings(ctx, org, want) },
			func() (*interfaces.AppSettings, error) { return b.LoadOrgSettings(ctx, org) }},
	} {
		if err := tc.save(); err != nil {
			t.Fatalf("%s: save: %v", tc.scope, err)
		}
		rows := readSectionRows(t, b, tc.scope, tc.id)
		for _, key := range settingsdoc.Default.Keys() {
			if r, ok := rows[key]; !ok || r.version != 1 || r.digest == nil {
				t.Errorf("%s: section %q row = %+v (present %v), want version 1 with a digest", tc.scope, key, r, ok)
			}
		}

		var blob []byte
		if err := b.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT data FROM %s WHERE %s = $1`, tc.table, tc.keyColumn), tc.key).Scan(&blob); err != nil {
			t.Fatalf("%s: read blob: %v", tc.scope, err)
		}
		var previousRelease interfaces.AppSettings
		if err := json.Unmarshal(blob, &previousRelease); err != nil {
			t.Fatalf("%s: decode blob: %v", tc.scope, err)
		}
		if !reflect.DeepEqual(&previousRelease, want) {
			t.Errorf("%s: the previous release reads\n %+v\nwant\n %+v", tc.scope, previousRelease, *want)
		}

		got, err := tc.load()
		if err != nil {
			t.Fatalf("%s: load: %v", tc.scope, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: loaded\n %+v\nwant\n %+v", tc.scope, *got, *want)
		}
	}
}

// TestPostgres_SettingsSections_FallBackToTheBlobPerSection: a document only the release before wrote reads from
// the blob; once some sections have rows, those come from the rows and the rest still from the blob.
func TestPostgres_SettingsSections_FallBackToTheBlobPerSection(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)

	blob, _ := json.Marshal(sampleSettings())
	mustExec(t, b, `INSERT INTO user_settings (user_id, data) VALUES ($1, $2)`, user, blob)
	got, err := b.LoadUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, sampleSettings()) {
		t.Errorf("blob only: loaded %+v", *got)
	}

	// A row the blob still matches is read first: its data stands for the section even where it differs.
	mustExec(t, b, `
		INSERT INTO settings_sections (scope, scope_id, section, version, data, legacy_digest)
		SELECT 'user', $1, 'appearance', 1, '{"theme":"dracula","density":"compact","chatTextSize":"lg"}',
		       md5((data -> 'appearance')::text)
		  FROM user_settings WHERE user_id = $1`, user)
	got, err = b.LoadUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Appearance.Theme != "dracula" {
		t.Errorf("appearance theme = %q, want the row's dracula", got.Appearance.Theme)
	}
	if got.AI.SystemPromptSuffix != "Be brief." || got.Parser.MaxFileSizeMB != 20 {
		t.Errorf("the sections without rows did not come from the blob: %+v", *got)
	}
}

// TestPostgres_SettingsSections_BlobWrittenAfterTheRowWins: rolled back, the release before writes only the blob;
// rolled forward again, its change must not be hidden behind the older section row. The fields only the row kept
// survive, and sections the old release did not change still read from their rows.
func TestPostgres_SettingsSections_BlobWrittenAfterTheRowWins(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	if err := b.SaveUserSettings(ctx, user, sampleSettings()); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A field this binary does not know in two rows; the digests are of the blob, so the rows stay current.
	mustExec(t, b, `UPDATE settings_sections SET data = data || '{"future":1}' WHERE scope = 'user' AND scope_id = $1 AND section IN ('appearance', 'parser')`, user)
	// The release before changes the theme, rewriting the whole blob as it does.
	mustExec(t, b, `UPDATE user_settings SET data = jsonb_set(data, '{appearance,theme}', '"light"') WHERE user_id = $1`, user)
	// A change of the parser's row alone (nothing the old release wrote) must still be read.
	mustExec(t, b, `UPDATE settings_sections SET data = jsonb_set(data, '{maxFileSizeMB}', '30') WHERE scope = 'user' AND scope_id = $1 AND section = 'parser'`, user)

	got, err := b.LoadUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Appearance.Theme != "light" || got.Appearance.Density != "compact" {
		t.Errorf("appearance = %+v, want the blob's light theme", got.Appearance)
	}
	if got.Parser.MaxFileSizeMB != 30 {
		t.Errorf("parser = %+v, want the current row's 30", got.Parser)
	}

	if err := b.SaveUserSettings(ctx, user, got); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows := readSectionRows(t, b, "user", user)
	if theme := jsonField(t, rows["appearance"].data, "theme"); theme != "light" {
		t.Errorf("appearance row theme = %v after the write, want light", theme)
	}
	for _, key := range []string{"appearance", "parser"} {
		if jsonField(t, rows[key].data, "future") != 1.0 {
			t.Errorf("%s row lost the field this binary does not know: %s", key, rows[key].data)
		}
	}
}

// TestPostgres_SettingsSections_UpgradeOnRead: a section stored at an older version, in a row or in the blob, reads
// through its upgrades, and the next write stores it at the current version.
func TestPostgres_SettingsSections_UpgradeOnRead(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	b.settingsSections = settingsdoc.MustRegistry(settingsdoc.Section{Key: "appearance", Version: 2, Upgrades: map[int]settingsdoc.Upgrade{
		2: func(data json.RawMessage) (json.RawMessage, error) {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, err
			}
			if m["dense"] == true {
				m["density"] = "compact"
			}
			delete(m, "dense")
			return json.Marshal(m)
		},
	}})

	user := newSettingsTestUser(t, b)
	mustExec(t, b, `INSERT INTO user_settings (user_id, data) VALUES ($1, '{"version":1,"appearance":{"theme":"nord","dense":true}}')`, user)
	got, err := b.LoadUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Appearance.Density != "compact" || got.Appearance.Theme != "nord" {
		t.Errorf("from the blob: appearance = %+v, want nord and compact", got.Appearance)
	}

	other := newSettingsTestUser(t, b)
	mustExec(t, b, `INSERT INTO settings_sections (scope, scope_id, section, version, data) VALUES ('user', $1, 'appearance', 1, '{"theme":"nord","dense":true}')`, other)
	got, err = b.LoadUserSettings(ctx, other)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Appearance.Density != "compact" {
		t.Errorf("from a row: appearance = %+v, want compact", got.Appearance)
	}
	if err := b.SaveUserSettings(ctx, other, got); err != nil {
		t.Fatalf("save: %v", err)
	}
	row := readSectionRows(t, b, "user", other)["appearance"]
	if row.version != 2 || jsonField(t, row.data, "dense") != nil || jsonField(t, row.data, "density") != "compact" {
		t.Errorf("row after the write = v%d %s, want version 2 without dense", row.version, row.data)
	}
}

// TestPostgres_SettingsSections_NewerVersionIsNeverDowngraded: a section a newer release stored (here one that no
// longer writes the blob, so its rows carry no digest) reads as-is for the fields this binary knows, and a write
// keeps its version and every field this binary does not know, at any depth.
func TestPostgres_SettingsSections_NewerVersionIsNeverDowngraded(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	if err := b.SaveUserSettings(ctx, user, sampleSettings()); err != nil {
		t.Fatalf("save: %v", err)
	}
	mustExec(t, b, `UPDATE settings_sections SET version = 5, legacy_digest = NULL,
		data = '{"theme":"light","density":"compact","future":{"x":1}}'
		WHERE scope = 'user' AND scope_id = $1 AND section = 'appearance'`, user)
	mustExec(t, b, `UPDATE settings_sections SET legacy_digest = NULL,
		data = jsonb_set(data, '{providers,openai,topP}', '0.9')
		WHERE scope = 'user' AND scope_id = $1 AND section = 'ai'`, user)

	got, err := b.LoadUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Appearance.Theme != "light" {
		t.Errorf("theme = %q, want the newer row's light", got.Appearance.Theme)
	}
	got.Appearance.Theme = "dark"
	got.AI.SystemPromptSuffix = "Answer in English."
	if err := b.SaveUserSettings(ctx, user, got); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows := readSectionRows(t, b, "user", user)
	if r := rows["appearance"]; r.version != 5 || jsonField(t, r.data, "theme") != "dark" || jsonField(t, r.data, "future") == nil {
		t.Errorf("appearance row = v%d %s, want version 5, dark, future kept", r.version, r.data)
	}
	var ai struct {
		SystemPromptSuffix string                    `json:"systemPromptSuffix"`
		Providers          map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rows["ai"].data, &ai); err != nil {
		t.Fatal(err)
	}
	if ai.SystemPromptSuffix != "Answer in English." || ai.Providers["openai"]["topP"] != 0.9 {
		t.Errorf("ai row = %s, want the new suffix and topP kept", rows["ai"].data)
	}
}

// TestPostgres_SettingsSections_RollBackWithTheTransaction: the section rows join the caller's transaction like the
// blob does.
func TestPostgres_SettingsSections_RollBackWithTheTransaction(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	tx, err := b.BeginRLS(ctx, user)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	if err := b.SaveUserSettings(WithRLSTx(ctx, tx), user, sampleSettings()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("save: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rows := readSectionRows(t, b, "user", user); len(rows) != 0 {
		t.Errorf("%d section rows survived the rollback", len(rows))
	}
}

// TestPostgres_SettingsSections_SaveLocksTheBlobFirst: a save takes the blob row before the section rows, the order
// in which deleting the user or organisation cascades through them. Locking the sections first deadlocked a save
// against a concurrent erasure: here the "erasure" holds the blob row, and the waiting save must not hold the
// sections the erasure takes next.
func TestPostgres_SettingsSections_SaveLocksTheBlobFirst(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	if err := b.SaveUserSettings(ctx, user, sampleSettings()); err != nil {
		t.Fatalf("first save: %v", err)
	}

	eraser, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eraser.Rollback() }()
	if _, err := eraser.ExecContext(ctx, `SELECT 1 FROM user_settings WHERE user_id = $1 FOR UPDATE`, user); err != nil {
		t.Fatalf("lock the blob: %v", err)
	}

	saved := make(chan error, 1)
	go func() { saved <- b.SaveUserSettings(context.Background(), user, sampleSettings()) }()
	// Wait until the save is blocked on a lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the save never waited on the blob row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := eraser.ExecContext(ctx, `SELECT 1 FROM settings_sections WHERE scope = 'user' AND scope_id = $1 FOR UPDATE NOWAIT`, user); err != nil {
		t.Errorf("the waiting save holds the section rows (a save against an erasure deadlocks): %v", err)
	}
	if err := eraser.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Errorf("save after the lock was released: %v", err)
	}
}

// TestPostgres_SettingsSections_SaveDoesNotDeadlockAnErasure: a save that inserts a section row the scope did not
// have (the first save after v35 of a user with only a blob, or a new section) checks the foreign key on the users
// row. Holding the blob row while an erasure held the users row and waited on the blob deadlocked; the save now locks
// the users row first, so the erasure simply waits for it.
func TestPostgres_SettingsSections_SaveDoesNotDeadlockAnErasure(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	// A user from before v35: the blob, and an "appearance" row at the version before.
	mustExec(t, b, `INSERT INTO user_settings (user_id, data) VALUES ($1, '{"version":1,"appearance":{"theme":"nord"}}')`, user)
	mustExec(t, b, `INSERT INTO settings_sections (scope, scope_id, section, version, data) VALUES ('user', $1, 'appearance', 1, '{"theme":"nord"}')`, user)

	entered, release := make(chan struct{}), make(chan struct{})
	b.settingsSections = settingsdoc.MustRegistry(settingsdoc.Section{Key: "appearance", Version: 2, Upgrades: map[int]settingsdoc.Upgrade{
		2: func(data json.RawMessage) (json.RawMessage, error) {
			close(entered) // inside the save: the owner, blob and section rows are locked
			<-release
			return data, nil
		},
	}})

	saved := make(chan error, 1)
	go func() { saved <- b.SaveUserSettings(context.Background(), user, sampleSettings()) }()
	<-entered
	erased := make(chan error, 1)
	go func() { erased <- b.DeleteUser(context.Background(), user) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%users%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the erasure never waited on the save")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if err := <-saved; err != nil {
		t.Errorf("save: %v", err)
	}
	if err := <-erased; err != nil {
		t.Errorf("erasure: %v", err)
	}
}

// TestPostgres_SettingsSections_GoWithTheirUserAndOrg: the rows of a user and of an organisation are deleted with
// them, as their blob rows are.
func TestPostgres_SettingsSections_GoWithTheirUserAndOrg(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user := newSettingsTestUser(t, b)
	org := "settings-sections-org-" + user
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Sections", OwnerID: user}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	if err := b.SaveUserSettings(ctx, user, sampleSettings()); err != nil {
		t.Fatalf("save user: %v", err)
	}
	if err := b.SaveOrgSettings(ctx, org, sampleSettings()); err != nil {
		t.Fatalf("save org: %v", err)
	}
	if err := b.DeleteOrg(ctx, org); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if err := b.DeleteUser(ctx, user); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for scope, id := range map[string]string{"user": user, "org": org} {
		if rows := readSectionRows(t, b, scope, id); len(rows) != 0 {
			t.Errorf("%s: %d section rows survived the delete", scope, len(rows))
		}
	}
}
