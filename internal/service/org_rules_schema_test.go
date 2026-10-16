package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/models"
)

// legacyRow is a row as every release before custom-rule kinds wrote it: the 12-key marshal, no kind or version.
const legacyRow = `{"id":"acme-no-set","name":"Acme bans SET","description":"House policy","severity":"error","category":"Style","rawTypeMatch":"^SET$","nameMatch":"","propertyHas":null,"propertyNot":null,"typeMatch":"","suggestion":""}`

func seedRow(t *testing.T, b *testutil.FakeBackend, org, rowID, ruleID, config string, enabled bool) *storageif.OrgCustomRule {
	t.Helper()
	row := &storageif.OrgCustomRule{ID: rowID, OrgID: org, RuleID: ruleID, Config: json.RawMessage(config), Enabled: enabled}
	if err := b.SaveOrgCustomRule(context.Background(), row); err != nil {
		t.Fatalf("seed %s: %v", rowID, err)
	}
	return row
}

func storedConfig(t *testing.T, b *testutil.FakeBackend, org, rowID string) map[string]any {
	t.Helper()
	rows, err := b.ListOrgCustomRules(context.Background(), org, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == rowID {
			var m map[string]any
			if err := json.Unmarshal(r.Config, &m); err != nil {
				t.Fatalf("stored config: %v", err)
			}
			return m
		}
	}
	t.Fatalf("row %s is not stored", rowID)
	return nil
}

// ReadOrgRule reads a stored row the way the analysis does (tolerantly): what runs, what runs with a warning,
// and what is skipped with the problem the rule list shows.
func TestReadOrgRule(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		runs         bool
		warning      string // a warning reason the rule runs with
		problem      string // the problem's reason
		tooNew       bool
	}{
		{name: "legacy row", config: legacyRow, runs: true},
		{name: "kind and version spelled", config: `{"kind":"block","schemaVersion":1,"id":"acme-no-set","rawTypeMatch":"^SET$"}`, runs: true},
		{name: "unknown field", config: `{"id":"acme-no-set","rawTypeMatch":"^SET$","nameMatches":"typo"}`, runs: true, warning: analyzer.ReasonUnknownField},
		{name: "no id", config: `{"rawTypeMatch":"^SET$"}`, runs: true, warning: analyzer.ReasonMissingID},
		{name: "built-in id", config: `{"id":"dead-code","rawTypeMatch":"^SET$"}`, runs: true, warning: analyzer.ReasonReservedID},
		{name: "future kind", config: `{"kind":"flow","schemaVersion":1,"id":"acme-no-set","when":{"lacks":"Retry"}}`, problem: analyzer.ReasonUnknownKind, tooNew: true},
		{name: "future version", config: `{"schemaVersion":2,"id":"acme-no-set","rawTypeMatch":"^SET$"}`, problem: analyzer.ReasonUnsupportedVersion, tooNew: true},
		// ADR 0015 lets a later kind or schemaVersion give a field another JSON type: the row is still a newer
		// release's rule, not INVALID_JSON (which an edit may overwrite).
		{name: "future version retypes a field", config: `{"kind":"block","schemaVersion":2,"id":"acme-no-set","severity":"error","nameMatch":["^Set","^Get"],"anchor":"start"}`, problem: analyzer.ReasonUnsupportedVersion, tooNew: true},
		{name: "future kind retypes a field", config: `{"kind":"flow","id":"acme-no-set","severity":{"level":"error"}}`, problem: analyzer.ReasonUnknownKind, tooNew: true},
		{name: "retyped kind of a future version", config: `{"kind":["block"],"schemaVersion":2,"id":"acme-no-set"}`, problem: analyzer.ReasonUnsupportedVersion, tooNew: true},
		{name: "wrong JSON type at version 1", config: `{"kind":"block","schemaVersion":1,"id":"acme-no-set","nameMatch":["^Set"]}`, problem: analyzer.ReasonInvalidJSON},
		{name: "wrong JSON type, negative version", config: `{"schemaVersion":-1,"id":"acme-no-set","nameMatch":["^Set"]}`, problem: analyzer.ReasonInvalidJSON},
		{name: "negative version", config: `{"schemaVersion":-1,"id":"acme-no-set"}`, problem: analyzer.ReasonUnsupportedVersion},
		{name: "legacy invalid severity", config: `{"id":"acme-no-set","severity":"critical","rawTypeMatch":"^SET$"}`, problem: analyzer.ReasonInvalidSeverity},
		{name: "wrong JSON type", config: `{"id":"acme-no-set","severity":3}`, problem: analyzer.ReasonInvalidJSON},
		{name: "not an object", config: `"acme"`, problem: analyzer.ReasonInvalidJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ReadOrgRule(&storageif.OrgCustomRule{ID: "row", OrgID: "org", RuleID: "acme-no-set", Config: json.RawMessage(tc.config)})
			if (r.Rule != nil) != tc.runs {
				t.Fatalf("compiled = %v, want %v (problem %v)", r.Rule != nil, tc.runs, r.Problem)
			}
			if tc.problem == "" && r.Problem != nil {
				t.Errorf("problem %v, want none", r.Problem)
			}
			if tc.problem != "" && (r.Problem == nil || r.Problem.Reason != tc.problem) {
				t.Errorf("problem = %v, want reason %s", r.Problem, tc.problem)
			}
			if r.TooNew() != tc.tooNew {
				t.Errorf("TooNew = %v, want %v", r.TooNew(), tc.tooNew)
			}
			var warned []string
			for _, w := range r.Warnings {
				warned = append(warned, w.Reason)
			}
			if tc.warning != "" && !strings.Contains(strings.Join(warned, ","), tc.warning) {
				t.Errorf("warnings %v, want %s", warned, tc.warning)
			}
			if tc.problem == analyzer.ReasonInvalidJSON && r.Config.ID != "acme-no-set" {
				t.Errorf("an undecodable row's config id = %q, want the row's rule id", r.Config.ID)
			}
			if tc.tooNew && r.Config.ID != "acme-no-set" {
				t.Errorf("a newer release's row's config id = %q, want the row's rule id", r.Config.ID)
			}
		})
	}
}

// The org layer of a rule profile skips the rules the analysis cannot run, and logs each stored rule's problems
// once per content: not on every resolve, but again after an edit.
func TestCompileOrgRules_SkipsAndLogsOncePerContent(t *testing.T) {
	org := "org-log-" + t.Name()
	rows := []*storageif.OrgCustomRule{
		{ID: "r-ok", OrgID: org, RuleID: "ok", Config: json.RawMessage(legacyRow)},
		{ID: "r-typo", OrgID: org, RuleID: "typo", Config: json.RawMessage(`{"id":"typo","rawTypeMatch":"^SET$","nameMatches":"x"}`)},
		{ID: "r-future", OrgID: org, RuleID: "future", Config: json.RawMessage(`{"kind":"flow","id":"future"}`)},
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ids := func(rules []analyzer.Rule) string {
		var out []string
		for _, r := range rules {
			out = append(out, r.ID())
		}
		return strings.Join(out, ",")
	}
	if got := ids(compileOrgRules(org, rows)); got != "acme-no-set,typo" {
		t.Errorf("compiled %q, want the legacy and the tolerated rule only", got)
	}
	first := buf.String()
	for _, want := range []string{
		`msg="rule profile: stored custom rule warning"`, "ruleId=typo", "reason=UNKNOWN_FIELD", "field=nameMatches",
		`msg="rule profile: stored custom rule skipped"`, "ruleId=future", "reason=UNKNOWN_KIND",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first resolve's log lacks %s:\n%s", want, first)
		}
	}
	if strings.Contains(first, "ruleId=ok") {
		t.Errorf("a clean rule was logged:\n%s", first)
	}

	buf.Reset()
	compileOrgRules(org, rows)
	if buf.Len() != 0 {
		t.Errorf("a second resolve logged again:\n%s", buf.String())
	}
	rows[1].Config = json.RawMessage(`{"id":"typo","rawTypeMatch":"^SET$","nameMatchez":"x"}`)
	compileOrgRules(org, rows)
	if got := buf.String(); !strings.Contains(got, "field=nameMatchez") || strings.Contains(got, "ruleId=future") {
		t.Errorf("after an edit the log = %q, want the edited rule's warning only", got)
	}
}

func asCustomRuleError(t *testing.T, err error) *analyzer.CustomRuleError {
	t.Helper()
	var ce *analyzer.CustomRuleError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *analyzer.CustomRuleError", err, err)
	}
	return ce
}

func wantConflict(t *testing.T, err error) *apperr.Error {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Kind.Status() != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 apperr", err)
	}
	return ae
}

// SaveRule writes strictly and normalized, keeps what it does not know, and never changes a rule's id or
// overwrites what a newer release wrote.
func TestOrgRuleService_SaveRule(t *testing.T) {
	ctx := context.Background()
	const org = "org-a"
	b := testutil.NewFakeBackend()
	b.SeedOrgs("org-a", "org-b") // a rule needs its organisation
	svc := NewOrgRuleService(b, b, nil)
	rule := func(id string) *analyzer.CustomRuleConfig {
		return &analyzer.CustomRuleConfig{ID: id, Name: id, Severity: "warning", Category: "Style", RawTypeMatch: "^SET$"}
	}

	t.Run("stores kind and schemaVersion first", func(t *testing.T) {
		saved, err := svc.SaveRule(ctx, org, OrgRuleSave{Config: rule("fresh")})
		if err != nil {
			t.Fatal(err)
		}
		if saved.Row.ID == "" || !saved.Row.Enabled || saved.Rule == nil {
			t.Fatalf("saved = %+v", saved.Row)
		}
		if !bytes.HasPrefix(saved.Row.Config, []byte(`{"kind":"block","schemaVersion":1,"id":"fresh"`)) {
			t.Errorf("stored config = %s", saved.Row.Config)
		}
	})

	t.Run("strict check refuses", func(t *testing.T) {
		unknown := rule("typo")
		if err := json.Unmarshal([]byte(`{"id":"typo","nameMatches":"x"}`), unknown); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name          string
			cfg           *analyzer.CustomRuleConfig
			field, reason string
		}{
			{"unknown field", unknown, "nameMatches", analyzer.ReasonUnknownField},
			{"missing id", rule(""), "id", analyzer.ReasonMissingID},
			{"built-in id", rule("dead-code"), "id", analyzer.ReasonReservedID},
			{"future kind", &analyzer.CustomRuleConfig{Kind: "flow", ID: "k"}, "kind", analyzer.ReasonUnknownKind},
			{"severity", &analyzer.CustomRuleConfig{ID: "sev", Severity: "critical"}, "severity", analyzer.ReasonInvalidSeverity},
		} {
			_, err := svc.SaveRule(ctx, org, OrgRuleSave{Config: tc.cfg})
			ce := asCustomRuleError(t, err)
			if ce.Field != tc.field || ce.Reason != tc.reason {
				t.Errorf("%s: field %q reason %q, want %q %q", tc.name, ce.Field, ce.Reason, tc.field, tc.reason)
			}
		}
		if rows, _ := b.ListOrgCustomRules(ctx, org, false); len(rows) != 1 {
			t.Errorf("refused saves stored rows: %d rows, want the one fresh rule", len(rows))
		}
	})

	t.Run("a stored built-in id stays editable", func(t *testing.T) {
		seedRow(t, b, org, "legacy-builtin", "dead-code", `{"id":"dead-code","severity":"warning","rawTypeMatch":"^SET$"}`, true)
		edit := rule("dead-code")
		edit.Severity = "error"
		if _, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "legacy-builtin", Config: edit}); err != nil {
			t.Fatalf("editing the stored built-in id: %v", err)
		}
		if got := storedConfig(t, b, org, "legacy-builtin")["severity"]; got != "error" {
			t.Errorf("severity = %v, want error", got)
		}
		if _, err := svc.SaveRule(ctx, org, OrgRuleSave{Config: edit}); err != nil {
			t.Errorf("saving by rule id over the stored built-in id: %v", err)
		}
	})

	t.Run("keeps the stored unknown fields", func(t *testing.T) {
		seedRow(t, b, org, "fwd", "fwd", `{"kind":"block","schemaVersion":1,"id":"fwd","severity":"info","rawTypeMatch":"^SET$","futureField":{"n":1}}`, true)
		listed, err := svc.List(ctx, org, false)
		if err != nil {
			t.Fatal(err)
		}
		var echo analyzer.CustomRuleConfig
		for _, r := range listed {
			if r.Row.ID == "fwd" {
				echo = r.Config.Normalized()
			}
		}
		// The editor sends back the echo (its known fields): that must pass the strict check.
		raw, _ := json.Marshal(echo)
		var sent analyzer.CustomRuleConfig
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatal(err)
		}
		sent.Severity = "warning"
		if _, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "fwd", Config: &sent}); err != nil {
			t.Fatalf("saving the echo: %v", err)
		}
		got := storedConfig(t, b, org, "fwd")
		if got["severity"] != "warning" || got["futureField"] == nil {
			t.Errorf("stored %v, want the edit with futureField kept", got)
		}
	})

	t.Run("a rule's id never changes", func(t *testing.T) {
		seedRow(t, b, org, "named", "named", legacyRow, true)
		_, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "named", Config: rule("renamed")})
		if ae := wantConflict(t, err); ae.Details["ruleId"] != "named" {
			t.Errorf("details = %v", ae.Details)
		}
		if got := storedConfig(t, b, org, "named")["id"]; got != "acme-no-set" {
			t.Errorf("the refused rename changed the row: id %v", got)
		}
		// Blanking the id is a missing id, not a rename.
		_, err = svc.SaveRule(ctx, org, OrgRuleSave{RowID: "named", Config: rule(" ")})
		if ce := asCustomRuleError(t, err); ce.Reason != analyzer.ReasonMissingID {
			t.Errorf("blank id: %v", err)
		}
	})

	t.Run("a newer release's rule is not overwritten", func(t *testing.T) {
		seedRow(t, b, org, "future", "future", `{"kind":"flow","schemaVersion":1,"id":"future","when":{"lacks":"Retry"}}`, true)
		_, err := svc.SaveRule(ctx, org, OrgRuleSave{Config: rule("future")})
		if ae := wantConflict(t, err); ae.Details["reason"] != analyzer.ReasonUnknownKind {
			t.Errorf("details = %v", ae.Details)
		}
		if got := storedConfig(t, b, org, "future")["kind"]; got != "flow" {
			t.Errorf("stored kind = %v, want flow", got)
		}
	})

	t.Run("a newer release's rule whose fields do not decode here is not overwritten", func(t *testing.T) {
		const v2 = `{"kind":"block","schemaVersion":2,"id":"house","severity":"error","nameMatch":["^Set","^Get"],"anchor":"start"}`
		seedRow(t, b, org, "house", "house", v2, true)
		for name, req := range map[string]OrgRuleSave{
			"by row id":  {RowID: "house", Config: rule("house")},
			"by rule id": {Config: rule("house")},
		} {
			_, err := svc.SaveRule(ctx, org, req)
			if ae := wantConflict(t, err); ae.Details["reason"] != analyzer.ReasonUnsupportedVersion {
				t.Errorf("%s: details = %v", name, ae.Details)
			}
		}
		stored := storedConfig(t, b, org, "house")
		if nameMatch, _ := stored["nameMatch"].([]any); stored["schemaVersion"] != 2.0 || stored["anchor"] != "start" || len(nameMatch) != 2 {
			t.Errorf("stored config = %v, want the newer release's rule untouched", stored)
		}
	})

	t.Run("a toggle leaves the config as stored", func(t *testing.T) {
		const broken = `{"id":"old-sev","severity":"critical","rawTypeMatch":"^SET$","extra":true}`
		seedRow(t, b, org, "old-sev", "old-sev", broken, true)
		off := false
		saved, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "old-sev", Enabled: &off})
		if err != nil {
			t.Fatalf("pausing a rule that no longer compiles: %v", err)
		}
		if saved.Row.Enabled || saved.Problem == nil || saved.Problem.Reason != analyzer.ReasonInvalidSeverity {
			t.Errorf("saved = %+v problem %v", saved.Row, saved.Problem)
		}
		rows, _ := b.ListOrgCustomRules(ctx, org, false)
		for _, r := range rows {
			if r.ID == "old-sev" && (r.Enabled || string(r.Config) != broken) {
				t.Errorf("stored %+v (%s), want paused with the config untouched", r, r.Config)
			}
		}
		// The future-kind rule can be paused too.
		if _, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "future", Enabled: &off}); err != nil {
			t.Errorf("pausing a newer release's rule: %v", err)
		}
	})

	t.Run("toggle errors", func(t *testing.T) {
		on := true
		for _, req := range []OrgRuleSave{{RowID: "old-sev"}, {Enabled: &on}} {
			if _, err := svc.SaveRule(ctx, org, req); err == nil {
				t.Errorf("toggle %+v: no error", req)
			} else if ae, ok := apperr.As(err); !ok || ae.Kind.Status() != http.StatusBadRequest {
				t.Errorf("toggle %+v: %v, want 400", req, err)
			}
		}
		_, err := svc.SaveRule(ctx, org, OrgRuleSave{RowID: "nope", Enabled: &on})
		if ae, ok := apperr.As(err); !ok || ae.Kind.Status() != http.StatusNotFound {
			t.Errorf("toggle of an unknown row: %v, want 404", err)
		}
		// Another org's row is unknown here as well.
		seedRow(t, b, "org-b", "theirs", "theirs", legacyRow, true)
		_, err = svc.SaveRule(ctx, org, OrgRuleSave{RowID: "theirs", Enabled: &on})
		if ae, ok := apperr.As(err); !ok || ae.Kind.Status() != http.StatusNotFound {
			t.Errorf("toggle of another org's row: %v, want 404", err)
		}
	})

	t.Run("the store's rename guard maps to 409", func(t *testing.T) {
		// A store that refuses the rename itself (the service's own check raced with a concurrent save).
		racing := NewOrgRuleService(renamingStore{b}, b, nil)
		_, err := racing.SaveRule(ctx, org, OrgRuleSave{Config: rule("race")})
		wantConflict(t, err)
	})
}

// renamingStore answers every save with the store's rename refusal.
type renamingStore struct{ *testutil.FakeBackend }

func (renamingStore) SaveOrgCustomRule(context.Context, *storageif.OrgCustomRule) error {
	return storageif.ErrOrgCustomRuleRenamed
}

// The rule tester checks the candidate strictly, as the save does.
func TestAnalysisService_TestRuleIsStrict(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := &models.FlowDocument{ID: "f", Name: "Main", Subflows: []models.Subflow{{ID: "main", Name: "Main",
		Blocks: []models.Block{{ID: "b1", Type: models.BlockTypeAction, RawType: "SET", Name: "Set"}}}}}
	var typo analyzer.CustomRuleConfig
	if err := json.Unmarshal([]byte(`{"id":"t","rawTypeMatch":"^SET$","nameMatches":"x"}`), &typo); err != nil {
		t.Fatal(err)
	}
	_, err = svc.TestRule(context.Background(), doc, typo)
	if ce := asCustomRuleError(t, err); ce.Reason != analyzer.ReasonUnknownField || !strings.HasPrefix(err.Error(), "invalid rule: ") {
		t.Errorf("err = %v", err)
	}
	findings, err := svc.TestRule(context.Background(), doc, analyzer.CustomRuleConfig{ID: "t", RawTypeMatch: "^SET$"})
	if err != nil || len(findings) != 1 {
		t.Errorf("a valid rule: %d findings, %v", len(findings), err)
	}
	// A test stores nothing, so a built-in rule's id is no reason to refuse one: a legacy rule saved under one stays
	// testable from its editor.
	findings, err = svc.TestRule(context.Background(), doc, analyzer.CustomRuleConfig{ID: "dead-code", RawTypeMatch: "^SET$"})
	if err != nil || len(findings) != 1 || findings[0].RuleID != "dead-code" {
		t.Errorf("a rule under a built-in id: %+v, %v", findings, err)
	}
}

// An undecodable row echoes the fields that decode, as before Phase 4a, so the editor's form and an old bundle's
// toggle (which posts the echo back) keep its matcher instead of saving a rule that matches every block.
func TestReadOrgRule_UndecodableRowEchoesWhatDecodes(t *testing.T) {
	r := ReadOrgRule(&storageif.OrgCustomRule{ID: "row", OrgID: "org", RuleID: "garbled",
		Config: json.RawMessage(`{"id":"other","severity":3,"rawTypeMatch":"^SET$","nameMatch":"x"}`)})
	if r.Problem == nil || r.Problem.Reason != analyzer.ReasonInvalidJSON || r.Rule != nil {
		t.Fatalf("problem %v, rule %v: want INVALID_JSON and no rule", r.Problem, r.Rule)
	}
	if r.Config.ID != "garbled" || r.Config.RawTypeMatch != "^SET$" || r.Config.NameMatch != "x" || r.Config.Severity != "" {
		t.Errorf("echo = %+v, want the row's id with rawTypeMatch and nameMatch kept", r.Config)
	}
}

// racingRuleStore lands race (a full save of the same rule) between a toggle's read of the org's rules and its write.
type racingRuleStore struct {
	*testutil.FakeBackend
	race func()
}

func (s *racingRuleStore) fire() {
	if race := s.race; race != nil {
		s.race = nil
		race()
	}
}

func (s *racingRuleStore) ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgCustomRule, error) {
	rows, err := s.FakeBackend.ListOrgCustomRules(ctx, orgID, enabledOnly)
	s.fire()
	return rows, err
}

// SetOrgCustomRuleEnabled is a toggle's only store call: the save lands just before it.
func (s *racingRuleStore) SetOrgCustomRuleEnabled(ctx context.Context, orgID, id string, enabled bool) (*storageif.OrgCustomRule, error) {
	s.fire()
	return s.FakeBackend.SetOrgCustomRuleEnabled(ctx, orgID, id, enabled)
}

// A pause (a toggle without config) racing a full save of the same rule wrote back the config it had read: the save
// landed between the pause's read and its write, and its edit was lost while both requests answered 200.
func TestOrgRuleService_PauseKeepsAConcurrentSave(t *testing.T) {
	ctx := context.Background()
	const org = "org-a"
	b := testutil.NewFakeBackend()
	b.SeedOrgs("org-a") // a rule needs its organisation
	seedRow(t, b, org, "row", "house", `{"kind":"block","schemaVersion":1,"id":"house","name":"house","severity":"warning","rawTypeMatch":"^SET$"}`, true)
	store := &racingRuleStore{FakeBackend: b}
	store.race = func() {
		on := true
		edit := &analyzer.CustomRuleConfig{ID: "house", Name: "house", Severity: "warning", RawTypeMatch: "^WAIT$"}
		if _, err := NewOrgRuleService(b, b, nil).SaveRule(ctx, org, OrgRuleSave{RowID: "row", Config: edit, Enabled: &on}); err != nil {
			t.Errorf("the concurrent save: %v", err)
		}
	}
	off := false
	paused, err := NewOrgRuleService(store, b, nil).SaveRule(ctx, org, OrgRuleSave{RowID: "row", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if store.race != nil {
		t.Fatal("the save did not land before the pause's write")
	}
	if got := storedConfig(t, b, org, "row")["rawTypeMatch"]; got != "^WAIT$" {
		t.Errorf("stored rawTypeMatch = %v, want the concurrent save's ^WAIT$", got)
	}
	rows, _ := b.ListOrgCustomRules(ctx, org, false)
	if len(rows) != 1 || rows[0].Enabled {
		t.Errorf("stored %+v, want the one rule paused", rows)
	}
	if paused.Row.Enabled || paused.Config.RawTypeMatch != "^WAIT$" {
		t.Errorf("the pause answered %+v (config %+v), want the stored rule paused", paused.Row, paused.Config)
	}
}
