package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/models"
)

// The settings file records each section's version (settingsdoc, ADR 0012).

func readSettingsFile(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("decode settings file: %v", err)
	}
	return top
}

func sectionVersionsOf(t *testing.T, top map[string]json.RawMessage) map[string]int {
	t.Helper()
	var v map[string]int
	if err := json.Unmarshal(top[sectionVersionsKey], &v); err != nil {
		t.Fatalf("decode %s: %v", sectionVersionsKey, err)
	}
	return v
}

func loadStoreAt(t *testing.T, path string, sections *settingsdoc.Registry) *SettingsStore {
	t.Helper()
	s := &SettingsStore{path: path, current: models.DefaultSettings(), sections: sections}
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

// TestSettingsStore_FileKeepsTheLayoutTheReleaseBeforeReads: the file is what the release before wrote (the document
// indented, in field order) with the section versions after it, and that release reads back what was written.
func TestSettingsStore_FileKeepsTheLayoutTheReleaseBeforeReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := loadStoreAt(t, path, nil)
	updated := *s.Get()
	updated.Appearance.Theme = models.ThemeNord
	updated.Analysis.Rules["dead-code"] = models.RuleConfig{Enabled: false, Severity: "warning"}
	if err := s.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var previousRelease models.AppSettings
	if err := json.Unmarshal(data, &previousRelease); err != nil {
		t.Fatalf("decode as the release before: %v", err)
	}
	// Compared as JSON: rule options read back as float64 where the defaults hold ints.
	read, _ := json.Marshal(previousRelease)
	want, _ := json.Marshal(s.Get())
	if string(read) != string(want) {
		t.Errorf("the release before reads\n %s\nwant\n %s", read, want)
	}

	old, err := json.MarshalIndent(s.Get(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	prefix := string(bytes.TrimSuffix(old, []byte("\n}"))) + ",\n  \"" + sectionVersionsKey + "\": {"
	if !strings.HasPrefix(string(data), prefix) {
		t.Errorf("the file departs from the old layout before the section versions:\n%s", data)
	}
	versions := sectionVersionsOf(t, readSettingsFile(t, path))
	for _, key := range settingsdoc.Default.Keys() {
		if versions[key] != 1 {
			t.Errorf("section %q recorded at version %d, want 1", key, versions[key])
		}
	}
}

// TestSettingsStore_UpgradesAnOlderSection: a file the release before wrote holds every section at the legacy
// version; a section whose version moved on reads through its upgrade, and the next write records the new version.
func TestSettingsStore_UpgradesAnOlderSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"appearance":{"theme":"light","dense":true},"parser":{"maxFileSizeMB":9}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sections := settingsdoc.MustRegistry(settingsdoc.Section{Key: "appearance", Version: 2, Upgrades: map[int]settingsdoc.Upgrade{
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
	s := loadStoreAt(t, path, sections)
	got := s.Get()
	if got.Appearance.Theme != "light" || got.Appearance.Density != "compact" || got.Parser.MaxFileSizeMB != 9 {
		t.Fatalf("loaded appearance %+v parser %+v, want light, compact, 9", got.Appearance, got.Parser)
	}

	if err := s.Update(context.Background(), *got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	top := readSettingsFile(t, path)
	if v := sectionVersionsOf(t, top)["appearance"]; v != 2 {
		t.Errorf("appearance recorded at version %d, want 2", v)
	}
	if strings.Contains(string(top["appearance"]), "dense") {
		t.Errorf("the upgraded-away field is still stored: %s", top["appearance"])
	}
}

// TestSettingsStore_KeepsWhatANewerBinaryWrote: after a downgrade, a write keeps a section's newer version, the
// fields this binary does not know at any depth, and whole sections it does not know, with their versions.
func TestSettingsStore_KeepsWhatANewerBinaryWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{
		"version": 1,
		"appearance": {"theme": "light", "density": "compact", "future": 1},
		"ai": {"activeProvider": "claude", "providers": {"claude": {"defaultModel": "m", "topP": 0.9}}},
		"notifications": {"email": true},
		"sectionVersions": {"appearance": 5, "ai": 1, "notifications": 2}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadStoreAt(t, path, nil)
	updated := *s.Get()
	if updated.Appearance.Theme != "light" {
		t.Fatalf("theme = %q, want light read from the newer section", updated.Appearance.Theme)
	}
	updated.Appearance.Theme = models.ThemeDark
	if err := s.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	top := readSettingsFile(t, path)
	versions := sectionVersionsOf(t, top)
	if versions["appearance"] != 5 || versions["notifications"] != 2 {
		t.Errorf("versions = %v, want appearance 5 and notifications 2 kept", versions)
	}
	var appearance map[string]any
	_ = json.Unmarshal(top["appearance"], &appearance)
	if appearance["theme"] != "dark" || appearance["future"] != 1.0 {
		t.Errorf("appearance = %s, want dark with future kept", top["appearance"])
	}
	if !strings.Contains(string(top["ai"]), `"topP": 0.9`) {
		t.Errorf("ai lost the provider field this binary does not know: %s", top["ai"])
	}
	if !strings.Contains(string(top["notifications"]), `"email": true`) {
		t.Errorf("the section this binary does not know was dropped: %v", top)
	}
}

// TestSettingsStore_SectionThatCannotBeReadIsQuarantined: a section that fails its upgrade is handled like an
// unparseable file: kept aside, defaults in its place.
func TestSettingsStore_SectionThatCannotBeReadIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"appearance":{"theme":"light"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sections := settingsdoc.MustRegistry(settingsdoc.Section{Key: "appearance", Version: 2, Upgrades: map[int]settingsdoc.Upgrade{
		2: func(json.RawMessage) (json.RawMessage, error) { return nil, errors.New("deliberate") },
	}})
	s := loadStoreAt(t, path, sections)
	if got := s.Get().Appearance.Theme; got != models.DefaultSettings().Appearance.Theme {
		t.Errorf("theme = %q, want the default", got)
	}
	entries, _ := os.ReadDir(dir)
	var quarantined bool
	for _, e := range entries {
		quarantined = quarantined || strings.Contains(e.Name(), ".corrupt-")
	}
	if !quarantined {
		t.Error("the unreadable file was not kept aside")
	}
}

// TestSettingsStore_DocumentVersionStaysOne: the document's own version is 1 whatever a client sends; sections
// carry the versions that change.
func TestSettingsStore_DocumentVersionStaysOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := loadStoreAt(t, path, nil)
	updated := *s.Get()
	updated.Version = 0
	if err := s.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if s.Get().Version != CurrentSettingsVersion || string(readSettingsFile(t, path)["version"]) != "1" {
		t.Errorf("document version = %d, want %d", s.Get().Version, CurrentSettingsVersion)
	}
}
