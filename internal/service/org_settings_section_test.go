package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// R-config-4: the section-level org settings write. It merges the fields it names into the stored section under the
// settings rows' lock, so concurrent writes to different fields both land, and a write carrying a stale revision
// is refused (409) instead of overwriting what it did not see.

func sectionValues(t *testing.T, kv map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = raw
	}
	return out
}

func newSectionTestService(t *testing.T) (*SystemService, *testutil.FakeBackend, *testutil.CountingNotifier, *eventbus.Recorder) {
	t.Helper()
	b := testutil.NewFakeBackend()
	b.Orgs = map[string]*storageif.Organisation{"org-1": {ID: "org-1"}}
	seed := models.DefaultSettings()
	seed.AI.AllowedProviders = []string{"claude"}
	seed.AI.SystemPromptSuffix = "keep me"
	seed.Analysis.Rules = map[string]models.RuleConfig{"dead-code": {Enabled: false, Severity: "info"}}
	if err := b.SaveOrgSettings(context.Background(), "org-1", settingsFromModel(seed)); err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	n := &testutil.CountingNotifier{}
	return NewSystemService(deploymentSettings(nil), nil, n, b, config.ModeCloud, nil, bus), b, n, rec
}

func storedOrgSettings(t *testing.T, b storageif.StorageBackend, orgID string) *models.AppSettings {
	t.Helper()
	s, err := b.LoadOrgSettings(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	return settingsToModel(s)
}

// TestPatchOrgSettingsSection_MergesTheNamedFieldsOnly: a write naming one field of ai keeps the section's other
// fields and the other sections; it answers with the section as written and its new revision, which a read returns
// too, and announces the change to the organisation.
func TestPatchOrgSettingsSection_MergesTheNamedFieldsOnly(t *testing.T) {
	sys, b, n, rec := newSectionTestService(t)
	ctx := context.Background()

	before, err := sys.GetOrgSettingsSection(ctx, "org-1", "ai")
	if err != nil {
		t.Fatal(err)
	}
	if before.Section != "ai" || before.Revision == "" || !strings.Contains(string(before.Values), `"allowedProviders":["claude"]`) {
		t.Fatalf("GetOrgSettingsSection = %+v, want the stored ai section with a revision", before)
	}

	out, err := sys.PatchOrgSettingsSection(ctx, "org-1", SettingsSectionPatch{
		Section: "ai", Values: sectionValues(t, map[string]any{"systemPromptSuffix": "new suffix"}), IfRevision: before.Revision,
	})
	if err != nil {
		t.Fatalf("PatchOrgSettingsSection: %v", err)
	}
	got := storedOrgSettings(t, b, "org-1")
	if got.AI.SystemPromptSuffix != "new suffix" || !slices.Equal(got.AI.AllowedProviders, []string{"claude"}) {
		t.Errorf("stored ai = %+v, want the new suffix and the allowlist kept", got.AI)
	}
	if got.Analysis.Rules["dead-code"].Severity != "info" {
		t.Errorf("another section changed: %+v", got.Analysis)
	}
	if out.Revision == "" || out.Revision == before.Revision {
		t.Errorf("revision after the write = %q, want a new one (was %q)", out.Revision, before.Revision)
	}
	after, err := sys.GetOrgSettingsSection(ctx, "org-1", "ai")
	if err != nil || after.Revision != out.Revision || string(after.Values) != string(out.Values) {
		t.Errorf("a read after the write = %+v, %v; want what the write answered (%+v)", after, err, out)
	}
	if ev := eventbus.Recorded[events.SettingsChanged](rec); len(ev) != 1 || ev[0] != (events.SettingsChanged{Scope: events.OrgSettings, ID: "org-1"}) {
		t.Errorf("announced %+v, want one org settings change", ev)
	}
	if em := n.Events(); len(em) != 1 || em[0].Audience != testutil.AudienceOrg || em[0].Target != "org-1" {
		t.Errorf("emitted %+v, want settings:changed to the organisation", em)
	}

	// An explicit null clears a field.
	if _, err := sys.PatchOrgSettingsSection(ctx, "org-1", SettingsSectionPatch{
		Section: "ai", Values: map[string]json.RawMessage{"allowedProviders": json.RawMessage("null")},
	}); err != nil {
		t.Fatal(err)
	}
	if got := storedOrgSettings(t, b, "org-1"); len(got.AI.AllowedProviders) != 0 || got.AI.SystemPromptSuffix != "new suffix" {
		t.Errorf("after clearing the allowlist: %+v, want it empty and the suffix kept", got.AI)
	}
}

// TestPatchOrgSettingsSection_ConcurrentWritesToDifferentFieldsBothLand is the item's first Done-when on the service:
// one write naming ai.allowedProviders and one naming ai.systemPromptSuffix, at the same time and without a
// precondition, both land and neither field reverts.
func TestPatchOrgSettingsSection_ConcurrentWritesToDifferentFieldsBothLand(t *testing.T) {
	sys, b, _, _ := newSectionTestService(t)
	concurrentSectionWrites(t, sys, b, context.Background, 20)
}

// concurrentSectionWrites runs rounds of two concurrent writes to different ai fields and checks both land.
func concurrentSectionWrites(t *testing.T, sys *SystemService, b storageif.StorageBackend, reqCtx func() context.Context, rounds int) {
	t.Helper()
	for round := range rounds {
		providers := []string{"openai"}
		if round%2 == 1 {
			providers = []string{"claude", "gemini"}
		}
		suffix := fmt.Sprintf("suffix %d", round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, values := range []map[string]json.RawMessage{
			sectionValues(t, map[string]any{"allowedProviders": providers}),
			sectionValues(t, map[string]any{"systemPromptSuffix": suffix}),
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = sys.PatchOrgSettingsSection(reqCtx(), "org-1", SettingsSectionPatch{Section: "ai", Values: values})
			}()
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		got := storedOrgSettings(t, b, "org-1")
		if !slices.Equal(got.AI.AllowedProviders, providers) || got.AI.SystemPromptSuffix != suffix {
			t.Fatalf("round %d: stored allowedProviders %v, systemPromptSuffix %q; want %v and %q (one write was lost)",
				round, got.AI.AllowedProviders, got.AI.SystemPromptSuffix, providers, suffix)
		}
	}
}

// TestPatchOrgSettingsSection_StaleRevisionConflicts is the item's second Done-when on the service: a write whose
// revision the section has moved past is refused with ErrSettingsConflict and changes nothing.
func TestPatchOrgSettingsSection_StaleRevisionConflicts(t *testing.T) {
	sys, b, _, _ := newSectionTestService(t)
	ctx := context.Background()
	read, err := sys.GetOrgSettingsSection(ctx, "org-1", "ai")
	if err != nil {
		t.Fatal(err)
	}
	// Another admin saves first.
	if _, err := sys.PatchOrgSettingsSection(ctx, "org-1", SettingsSectionPatch{
		Section: "ai", Values: sectionValues(t, map[string]any{"systemPromptSuffix": "theirs"}), IfRevision: read.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = sys.PatchOrgSettingsSection(ctx, "org-1", SettingsSectionPatch{
		Section: "ai", Values: sectionValues(t, map[string]any{"allowedProviders": []string{"openai"}}), IfRevision: read.Revision,
	})
	if !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("a write with a stale revision: %v, want ErrSettingsConflict", err)
	}
	if got := storedOrgSettings(t, b, "org-1"); !slices.Equal(got.AI.AllowedProviders, []string{"claude"}) || got.AI.SystemPromptSuffix != "theirs" {
		t.Errorf("a refused write changed the section: %+v", got.AI)
	}
}

// TestPatchOrgSettingsSection_Refusals: what the write cannot name, or values the section's rules refuse, are
// ErrSettingsInvalid (an unknown provider id is the allowlist's own *UnknownProvidersError), and nothing is stored.
func TestPatchOrgSettingsSection_Refusals(t *testing.T) {
	sys, b, _, _ := newSectionTestService(t)
	ctx := context.Background()
	want := storedOrgSettings(t, b, "org-1")
	for name, p := range map[string]SettingsSectionPatch{
		"unknown section":         {Section: "nope", Values: sectionValues(t, map[string]any{"x": 1})},
		"server-owned section":    {Section: "recentFiles", Values: sectionValues(t, map[string]any{"x": 1})},
		"the document version":    {Section: "version", Values: sectionValues(t, map[string]any{"x": 1})},
		"unknown field":           {Section: "ai", Values: sectionValues(t, map[string]any{"dailyBudget": 5})},
		"no field":                {Section: "ai", Values: map[string]json.RawMessage{}},
		"wrong type":              {Section: "ai", Values: sectionValues(t, map[string]any{"allowedProviders": 5})},
		"suffix over its cap":     {Section: "ai", Values: sectionValues(t, map[string]any{"systemPromptSuffix": strings.Repeat("x", MaxSystemPromptSuffixChars+1)})},
		"profile over its cap":    {Section: "profile", Values: sectionValues(t, map[string]any{"bio": strings.Repeat("x", profileBioMaxChars+1)})},
		"unknown active provider": {Section: "ai", Values: sectionValues(t, map[string]any{"activeProvider": "nope"})},
	} {
		if _, err := sys.PatchOrgSettingsSection(ctx, "org-1", p); !errors.Is(err, ErrSettingsInvalid) {
			t.Errorf("%s: %v, want ErrSettingsInvalid", name, err)
		}
	}
	var unknown *UnknownProvidersError
	_, err := sys.PatchOrgSettingsSection(ctx, "org-1", SettingsSectionPatch{Section: "ai", Values: sectionValues(t, map[string]any{"allowedProviders": []string{"claude", "nope"}})})
	if !errors.As(err, &unknown) || !slices.Equal(unknown.IDs, []string{"nope"}) {
		t.Errorf("an unknown provider id: %v, want *UnknownProvidersError naming it", err)
	}
	if _, err := sys.GetOrgSettingsSection(ctx, "org-1", "recentFiles"); !errors.Is(err, ErrSettingsInvalid) {
		t.Errorf("reading a server-owned section: %v, want ErrSettingsInvalid", err)
	}
	got := storedOrgSettings(t, b, "org-1")
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("a refused write changed the document:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestPatchOrgSettingsSection_Postgres runs the Done-whens against Postgres, each write in its own request
// transaction as the route runs it: concurrent writes to different ai fields both land, round after round, and a
// stale revision conflicts.
func TestPatchOrgSettingsSection_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	if err := pg.CreateUser(ctx, &storageif.User{ID: "sec-admin", Email: "sec-admin@example.com", Password: "hash", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SaveOrg(ctx, &storageif.Organisation{ID: "org-1", Name: "Sections", OwnerID: "sec-admin",
		Members: []storageif.OrgMember{{UserID: "sec-admin", Role: auth.RoleAdmin}}}); err != nil {
		t.Fatal(err)
	}
	sys := NewSystemService(deploymentSettings(nil), nil, NilNotifier{}, pg, config.ModeCloud, nil, nil)

	// Each write runs in the admin's request transaction, committed when the write returns.
	inRequest := func(fn func(ctx context.Context) error) error {
		reqCtx := auth.WithActor(ctx, userActor("sec-admin", auth.RoleMember))
		reqCtx, scope := storagedb.WithLazyRLS(reqCtx, pg, "sec-admin", false)
		if err := fn(reqCtx); err != nil {
			_ = scope.Rollback()
			return err
		}
		return scope.Commit()
	}
	patch := func(p SettingsSectionPatch) (SettingsSectionState, error) {
		var out SettingsSectionState
		err := inRequest(func(ctx context.Context) error {
			var err error
			out, err = sys.PatchOrgSettingsSection(ctx, "org-1", p)
			return err
		})
		return out, err
	}

	for round := range 10 {
		providers := []string{"openai"}
		if round%2 == 1 {
			providers = []string{"claude", "gemini"}
		}
		suffix := fmt.Sprintf("suffix %d", round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, values := range []map[string]json.RawMessage{
			sectionValues(t, map[string]any{"allowedProviders": providers}),
			sectionValues(t, map[string]any{"systemPromptSuffix": suffix}),
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = patch(SettingsSectionPatch{Section: "ai", Values: values})
			}()
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		got := storedOrgSettings(t, pg, "org-1")
		if !slices.Equal(got.AI.AllowedProviders, providers) || got.AI.SystemPromptSuffix != suffix {
			t.Fatalf("round %d: stored allowedProviders %v, systemPromptSuffix %q; want %v and %q (one write was lost)",
				round, got.AI.AllowedProviders, got.AI.SystemPromptSuffix, providers, suffix)
		}
	}

	read, err := sys.GetOrgSettingsSection(ctx, "org-1", "ai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := patch(SettingsSectionPatch{Section: "ai", Values: sectionValues(t, map[string]any{"systemPromptSuffix": "theirs"}), IfRevision: read.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := patch(SettingsSectionPatch{Section: "ai", Values: sectionValues(t, map[string]any{"allowedProviders": []string{"openai"}}), IfRevision: read.Revision}); !errors.Is(err, ErrSettingsConflict) {
		t.Errorf("a stale revision on Postgres: %v, want ErrSettingsConflict", err)
	}
}
