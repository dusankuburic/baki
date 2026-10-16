package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/models"
)

// sectionJSON is doc's top-level section key as encoded.
func sectionJSON(t *testing.T, doc *models.AppSettings, key string) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	return string(top[key])
}

// Every section of the document is kept when a write leaves it out, and only then: a section added to AppSettings is
// covered without a change here.
func TestSettingsSections_EverySectionIsKeptWhenUnnamed(t *testing.T) {
	stored := models.DefaultSettings()
	stored.Profile = models.ProfileSettings{Bio: "kept", JobTitle: "kept", Timezone: "Europe/Belgrade"}
	stored.General.FirstRunCompleted = true
	stored.RecentFiles = []models.RecentFile{{Path: "/kept.xml", Name: "kept.xml", LastOpen: time.Unix(1, 0).UTC()}}
	keys := settingsdoc.DocumentKeys(models.AppSettings{})
	for _, unnamed := range keys {
		if unnamed == settingsdoc.DocVersionKey {
			continue
		}
		t.Run(unnamed, func(t *testing.T) {
			named := SettingsSections{}
			for _, k := range keys {
				named[k] = k != unnamed
			}
			written := models.AppSettings{Version: 1}
			named.keepUnnamed(&written, stored.Clone())
			for _, k := range keys {
				want := sectionJSON(t, &models.AppSettings{}, k)
				if k == unnamed {
					want = sectionJSON(t, stored, k)
				}
				if k == settingsdoc.DocVersionKey {
					want = "1"
				}
				if got := sectionJSON(t, &written, k); got != want {
					t.Errorf("section %q = %s, want %s", k, got, want)
				}
			}
		})
	}
}

func TestSettingsSectionsOf(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"profile":{"bio":"x"},"appearance":null,"Layout":{},"version":1,"unknown":{}}`, []string{"profile", "appearance", "layout", "version"}},
		{`{}`, nil},
		{`null`, nil},
		{`{"recentFiles":[],"profile":{}}`, []string{"profile"}},
	} {
		got := SettingsSectionsOf([]byte(tc.body))
		if got == nil {
			t.Errorf("%s: nil, which names every section; want a set", tc.body)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.body, got, tc.want)
		}
		for _, k := range tc.want {
			if !got[k] {
				t.Errorf("%s: %v lacks %q", tc.body, got, k)
			}
		}
	}
}

// The recent files are the server's own list (AddRecentFile, RemoveRecentFile, ClearRecentFiles), and the web client
// echoes the whole document it loaded: a settings write that names them, with the list it had before a file was opened,
// must not take the list back.
func TestUpdateUserSettings_EchoedRecentFilesAreIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	file, err := storage.NewSettingsStoreAt(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := file.AddRecentFile(filepath.Join(t.TempDir(), "opened-after-load.txt"), 12); err != nil {
		t.Fatal(err)
	}
	sys := NewSystemService(file, nil, NilNotifier{}, nil, config.ModeLocal, nil, nil)
	if !slices.Contains(settingsdoc.DocumentKeys(models.AppSettings{}), serverOwnedSettingsKey) {
		t.Fatalf("%q is no longer a key of the settings document: the guard protects nothing", serverOwnedSettingsKey)
	}

	body := []byte(`{"version":1,"appearance":{"theme":"light"},"recentFiles":[]}`) // loaded before the file was opened
	var write models.AppSettings
	if err := json.Unmarshal(body, &write); err != nil {
		t.Fatal(err)
	}
	if err := sys.UpdateUserSettings(ctx, "", write, SettingsSectionsOf(body)); err != nil {
		t.Fatalf("UpdateUserSettings: %v", err)
	}
	if got := file.Get(); got.Appearance.Theme != "light" || len(got.RecentFiles) != 1 {
		t.Errorf("theme %q, %d recent files; want the theme written and the server's one recent file kept",
			got.Appearance.Theme, len(got.RecentFiles))
	}
}

// On the desktop the settings file also holds the recent files, which the server keeps: a settings write that leaves
// them out (or the profile) keeps them, in memory and in the file.
func TestUpdateUserSettings_DesktopFileKeepsUnnamedSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	file, err := storage.NewSettingsStoreAt(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := models.DefaultSettings()
	start.Profile.Bio = "on the desktop"
	if err := file.Update(ctx, *start); err != nil {
		t.Fatal(err)
	}
	if err := file.AddRecentFile(filepath.Join(t.TempDir(), "flow.txt"), 12); err != nil {
		t.Fatal(err)
	}
	sys := NewSystemService(file, nil, NilNotifier{}, nil, config.ModeLocal, nil, nil)

	body := []byte(`{"version":1,"appearance":{"theme":"light"}}`)
	var write models.AppSettings
	if err := json.Unmarshal(body, &write); err != nil {
		t.Fatal(err)
	}
	if err := sys.UpdateUserSettings(ctx, "", write, SettingsSectionsOf(body)); err != nil {
		t.Fatalf("UpdateUserSettings: %v", err)
	}
	reopened, err := storage.NewSettingsStoreAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]*models.AppSettings{"in memory": file.Get(), "in the file": reopened.Get()} {
		if got.Appearance.Theme != "light" || got.Profile.Bio != "on the desktop" || len(got.RecentFiles) != 1 || got.Layout.SidebarWidth != start.Layout.SidebarWidth {
			t.Errorf("%s: theme %q, bio %q, %d recent files, sidebar %d; want the theme written and the rest kept",
				name, got.Appearance.Theme, got.Profile.Bio, len(got.RecentFiles), got.Layout.SidebarWidth)
		}
	}
}
