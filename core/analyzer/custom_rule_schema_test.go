package analyzer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
)

// customRuleConfigV0 is CustomRuleConfig as every release before kind and schemaVersion decoded it: the rollback
// reference (C11).
type customRuleConfigV0 struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Severity     string            `json:"severity"`
	Category     string            `json:"category"`
	RawTypeMatch string            `json:"rawTypeMatch"`
	NameMatch    string            `json:"nameMatch"`
	PropertyHas  map[string]string `json:"propertyHas"`
	PropertyNot  map[string]string `json:"propertyNot"`
	TypeMatch    string            `json:"typeMatch"`
	Suggestion   string            `json:"suggestion"`
	AutoFix      string            `json:"autoFix,omitempty"`
}

// acmeRowV0 is the bytes a stored org row holds today (survey §1.11): the 12-key marshal, no kind or version.
const acmeRowV0 = `{"id":"acme-no-set","name":"Acme bans SET","description":"House policy","severity":"error",` +
	`"category":"Style","rawTypeMatch":"^SET$","nameMatch":"","propertyHas":null,"propertyNot":null,"typeMatch":"","suggestion":""}`

// Pinned digests of acmeRowV0. RuleDigest is today's value (caches, the scanner's skip key); MatchDigest is what
// baselines persist as the rule's revision. Neither may ever change for this rule.
const (
	acmeRuleDigest  = "617f98157a643508"
	acmeMatchDigest = "9b7bed4fc516fa2a"
)

func acmeConfig() CustomRuleConfig {
	return CustomRuleConfig{ID: "acme-no-set", Name: "Acme bans SET", Description: "House policy",
		Severity: "error", Category: "Style", RawTypeMatch: "^SET$"}
}

func mustCompile(t *testing.T, cfg CustomRuleConfig, mode CustomRuleMode) (*CustomRule, []*CustomRuleError) {
	t.Helper()
	r, warnings, err := CompileCustomRule(cfg, mode)
	if err != nil {
		t.Fatalf("compile %q: %v", cfg.ID, err)
	}
	return r, warnings
}

func reasons(problems []*CustomRuleError) []string {
	var out []string
	for _, p := range problems {
		out = append(out, p.Reason)
	}
	return out
}

func decodeOne(t *testing.T, raw string) CustomRuleConfig {
	t.Helper()
	cfg, err := DecodeCustomRule([]byte(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return cfg
}

// C1 + C2: today's stored row reads as a block v1 rule with nothing unknown, and every spelling of it (legacy,
// kind and version written out, the write encoding) has today's digest.
func TestCustomRule_LegacyRowKeepsItsDigest(t *testing.T) {
	cfg := decodeOne(t, acmeRowV0)
	if got := cfg.UnknownFields(); got != nil {
		t.Errorf("unknown fields = %v, want none", got)
	}
	if n := cfg.Normalized(); n.Kind != CustomRuleKindBlock || n.SchemaVersion != 1 {
		t.Errorf("Normalized() = kind %q version %d, want block/1", n.Kind, n.SchemaVersion)
	}
	encoded, err := EncodeCustomRule(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(`{"kind":"block","schemaVersion":1,"id":"acme-no-set",`)) {
		t.Errorf("write encoding = %s, want kind and schemaVersion first", encoded)
	}
	spellings := map[string]CustomRuleConfig{
		"literal":         acmeConfig(),
		"stored row":      cfg,
		"normalized":      cfg.Normalized(),
		"explicit fields": decodeOne(t, `{"kind":"block","schemaVersion":1,`+acmeRowV0[1:]),
		"write encoding":  decodeOne(t, string(encoded)),
		"unknown field":   decodeOne(t, `{"futureHint":{"a":[1,2]},`+acmeRowV0[1:]),
	}
	for name, c := range spellings {
		r, _ := mustCompile(t, c, CustomRuleTolerant)
		if r.RuleDigest() != acmeRuleDigest {
			t.Errorf("%s: RuleDigest = %s, want %s (a moved digest invalidates every cache key and scanner skip)",
				name, r.RuleDigest(), acmeRuleDigest)
		}
		if r.MatchDigest() != acmeMatchDigest {
			t.Errorf("%s: MatchDigest = %s, want %s (baselines persist it)", name, r.MatchDigest(), acmeMatchDigest)
		}
	}
}

// C3 + C4: an unknown key is an error in Strict and a warning in Tolerant; a known key in another case is known,
// exactly as encoding/json matches it.
func TestCustomRule_UnknownFields(t *testing.T) {
	cfg := decodeOne(t, `{"id":"r","nameMatches":"Foo","zeta":1,"RawTypeMatch":"^SET$"}`)
	if got, want := cfg.UnknownFields(), []string{"nameMatches", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("UnknownFields = %v, want %v", got, want)
	}
	if cfg.RawTypeMatch != "^SET$" {
		t.Errorf("RawTypeMatch = %q; a case-folded known key must still decode", cfg.RawTypeMatch)
	}

	_, _, err := CompileCustomRule(cfg, CustomRuleStrict)
	var ce *CustomRuleError
	if !errors.As(err, &ce) || ce.Reason != ReasonUnknownField || ce.Field != "nameMatches" || ce.Warning {
		t.Fatalf("strict: err = %#v, want UNKNOWN_FIELD on nameMatches", err)
	}
	if got := reasons(CheckCustomRule(cfg, CustomRuleStrict)); !slices.Equal(got, []string{ReasonUnknownField, ReasonUnknownField}) {
		t.Errorf("strict problems = %v, want one UNKNOWN_FIELD per key", got)
	}

	r, warnings := mustCompile(t, cfg, CustomRuleTolerant)
	if r == nil || len(warnings) != 2 || !warnings[0].Warning || warnings[0].Field != "nameMatches" ||
		!strings.Contains(warnings[0].Error(), `unknown field "nameMatches" ignored`) {
		t.Errorf("tolerant warnings = %v, want two naming the fields", warnings)
	}
}

// TestCustomRule_KeyMatchingParity checks isKnownCustomRuleKey against encoding/json itself: a key is known exactly
// when DisallowUnknownFields accepts it on the method-less struct.
func TestCustomRule_KeyMatchingParity(t *testing.T) {
	keys := []string{"id", "ID", "Id", "nameMatch", "NAMEMATCH", "namematch", "nameMatches", "name_match",
		"kind", "KIND", "Kind", "schemaversion", "ſeverity", "severity ", "", "unknown", "Unknown",
		"autofix", "propertyhas", "propertyHAS", "typeMatch", "type", "rules"}
	for _, f := range reflect.VisibleFields(customRuleConfigType) {
		keys = append(keys, f.Name, strings.ToUpper(f.Name))
	}
	for _, key := range keys {
		body, _ := json.Marshal(map[string]any{key: nil})
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		jsonKnows := dec.Decode(new(customRuleConfigFields)) == nil
		if got := isKnownCustomRuleKey(key); got != jsonKnows {
			t.Errorf("key %q: known = %v, encoding/json says %v", key, got, jsonKnows)
		}
		var cfg CustomRuleConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.UnknownFields()) == 0; got != jsonKnows {
			t.Errorf("key %q: decoded as known = %v, encoding/json says %v", key, got, jsonKnows)
		}
	}
}

// C14: every field has a deliberate JSON name, and every name is classified for MatchDigest, so a field added later
// has to decide whether it changes what the rule matches.
func TestCustomRule_FieldsAreNamedAndClassified(t *testing.T) {
	for _, f := range reflect.VisibleFields(customRuleConfigType) {
		if f.IsExported() && f.Tag.Get("json") == "" {
			t.Errorf("CustomRuleConfig.%s has no json tag", f.Name)
		}
	}
	matching := []string{"kind", "typeMatch", "rawTypeMatch", "nameMatch", "propertyHas", "propertyNot", "autoFix"}
	other := []string{"schemaVersion", "id", "name", "description", "severity", "category", "suggestion"}
	if got, want := slices.Sorted(slices.Values(customRuleKeys)), slices.Sorted(slices.Values(append(slices.Clone(matching), other...))); !slices.Equal(got, want) {
		t.Errorf("CustomRuleConfig fields %v, classified %v: classify the new field (MatchDigest or not)", got, want)
	}
	if got := jsonFieldNames(reflect.TypeFor[customRuleMatch]()); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(matching))) {
		t.Errorf("customRuleMatch hashes %v, want %v", got, matching)
	}
}

// TestCustomRule_DecodeParity: wherever CustomRuleConfig is the decoded value or an array element (stored rows,
// rules files, validate bodies) values and error messages are byte-identical to encoding/json without the method.
func TestCustomRule_DecodeParity(t *testing.T) {
	inputs := []string{
		acmeRowV0, `{"severity":3}`, `{"id":"a","severity":3,"name":4}`, `{"propertyHas":{"a":1}}`,
		`{"propertyHas":"x"}`, `{"propertyNot":[1]}`, `{"id":{"x":1}}`, `{"ID":"a","Severity":true}`,
		`{"kind":7}`, `{"schemaVersion":"1"}`, `{"schemaVersion":1.5}`, `{"schemaVersion":-1}`, `"str"`, `7`,
		`true`, `[]`, `null`, `{}`, `{"propertyHas":null,"id":null}`, `{"RAWTYPEMATCH":"a","rawTypeMatch":"b"}`,
		`{"nameMatch":"x","NameMatch":"y","ſeverity":"error"}`, `{"id":"a",}`, `{"id":"a"`, ``,
	}
	normalize := func(s string) string { return strings.ReplaceAll(s, "customRuleConfigFields", "CustomRuleConfig") }
	for _, in := range inputs {
		for _, wrap := range []string{"%s", "[%s]", `[{"id":"first"},%s]`} {
			body := strings.Replace(wrap, "%s", in, 1)
			var got []CustomRuleConfig
			var want []customRuleConfigFields
			isArray := strings.HasPrefix(wrap, "[")
			var gotErr, wantErr error
			if isArray {
				gotErr = json.Unmarshal([]byte(body), &got)
				wantErr = json.Unmarshal([]byte(body), &want)
			} else {
				got, want = make([]CustomRuleConfig, 1), make([]customRuleConfigFields, 1)
				gotErr = json.Unmarshal([]byte(body), &got[0])
				wantErr = json.Unmarshal([]byte(body), &want[0])
			}
			if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != normalize(wantErr.Error())) {
				t.Errorf("%s: error %v, encoding/json gives %v", body, gotErr, wantErr)
				continue
			}
			if gotErr != nil {
				continue
			}
			for i := range want {
				g := got[i]
				g.unknown = nil
				if !reflect.DeepEqual(customRuleConfigFields(g), want[i]) {
					t.Errorf("%s: entry %d decoded %+v, encoding/json gives %+v", body, i, g, want[i])
				}
			}
		}
	}
}

// TestCustomRule_DecodeMergesLikeEncodingJSON: decoding into a filled value merges, and null keeps it.
func TestCustomRule_DecodeMergesLikeEncodingJSON(t *testing.T) {
	for _, in := range []string{`null`, `{"propertyHas":{"new":"w"}}`, `{"propertyHas":null,"id":null}`, `{"name":"n"}`} {
		got := CustomRuleConfig{ID: "pre", PropertyHas: map[string]string{"old": "v"}, PropertyNot: map[string]string{"x": "y"}}
		want := customRuleConfigFields{ID: "pre", PropertyHas: map[string]string{"old": "v"}, PropertyNot: map[string]string{"x": "y"}}
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(in), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(customRuleConfigFields(got), want) {
			t.Errorf("%s: got %+v, want %+v", in, got, want)
		}
	}
	// A second decode reports its own unknown keys, not the first one's.
	var cfg CustomRuleConfig
	_ = json.Unmarshal([]byte(`{"id":"a","x":1}`), &cfg)
	_ = json.Unmarshal([]byte(`{"id":"a","y":1}`), &cfg)
	if got := cfg.UnknownFields(); !slices.Equal(got, []string{"y"}) {
		t.Errorf("UnknownFields after re-decode = %v, want [y]", got)
	}
}

// TestCustomRule_TypeErrorInsideAnotherStruct pins what encoding/json does to every Unmarshaler's type error: the
// outer struct's name, then the same field path as before.
func TestCustomRule_TypeErrorInsideAnotherStruct(t *testing.T) {
	var req struct {
		Config CustomRuleConfig   `json:"config"`
		Inline []CustomRuleConfig `json:"customRulesInline"`
	}
	err := json.Unmarshal([]byte(`{"config":{"id":"a","severity":3}}`), &req)
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) || !strings.HasSuffix(err.Error(), ".config.severity of type string") {
		t.Errorf("nested type error = %v", err)
	}
	err = json.Unmarshal([]byte(`{"customRulesInline":[{"propertyHas":{"k":2}}]}`), &req)
	if err == nil || !strings.HasSuffix(err.Error(), ".customRulesInline.propertyHas of type string") {
		t.Errorf("nested array type error = %v", err)
	}
	err = json.Unmarshal([]byte(`{"config":"x"}`), &req)
	if err == nil || !strings.Contains(err.Error(), ".config of type analyzer.CustomRuleConfig") {
		t.Errorf("nested value type error = %v", err)
	}
}

// TestCustomRule_StrictBodyDoesNotHideUnknownConfigFields: DisallowUnknownFields stops at a type with its own
// UnmarshalJSON, so the unknown key must reach CompileCustomRule(Strict) through the config.
func TestCustomRule_StrictBodyDoesNotHideUnknownConfigFields(t *testing.T) {
	var req struct {
		Config CustomRuleConfig `json:"config"`
	}
	dec := json.NewDecoder(strings.NewReader(`{"config":{"id":"a","nameMatch":"x","nameMatches":"y"}}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CompileCustomRule(req.Config, CustomRuleStrict); err == nil {
		t.Fatal("strict compile accepted an unknown config field")
	}
}

// C5 + C6 + C7: a kind or schemaVersion this build cannot run is refused in BOTH modes, never compiled into a rule
// that matches every block.
func TestCustomRule_KindAndVersion(t *testing.T) {
	cases := []struct {
		json   string
		reason string // "" = compiles
	}{
		{`{"id":"r","nameMatch":"x"}`, ""},
		{`{"id":"r","nameMatch":"x","kind":""}`, ""},
		{`{"id":"r","nameMatch":"x","kind":"block"}`, ""},
		{`{"id":"r","nameMatch":"x","kind":"flow"}`, ReasonUnknownKind},
		{`{"id":"r","nameMatch":"x","kind":"Block"}`, ReasonUnknownKind},
		{`{"id":"r","nameMatch":"x","schemaVersion":0}`, ""},
		{`{"id":"r","nameMatch":"x","schemaVersion":1}`, ""},
		{`{"id":"r","nameMatch":"x","schemaVersion":2}`, ReasonUnsupportedVersion},
		{`{"id":"r","nameMatch":"x","schemaVersion":-1}`, ReasonUnsupportedVersion},
		{`{"kind":"flow","schemaVersion":2,"id":"needs-retry","name":"x","severity":"error","when":{"lacks":"Retry"}}`, ReasonUnknownKind},
		{`{"schemaVersion":2,"id":"r","severity":"bogus","nameMatches":"x"}`, ReasonUnsupportedVersion},
	}
	for _, c := range cases {
		cfg := decodeOne(t, c.json)
		for _, mode := range []CustomRuleMode{CustomRuleStrict, CustomRuleTolerant} {
			r, _, err := CompileCustomRule(cfg, mode)
			if c.reason == "" {
				if err != nil {
					t.Errorf("%s mode %d: %v", c.json, mode, err)
				}
				continue
			}
			var ce *CustomRuleError
			if r != nil || !errors.As(err, &ce) || ce.Reason != c.reason || ce.Warning {
				t.Errorf("%s mode %d: rule %v err %v, want %s", c.json, mode, r, err, c.reason)
			}
			// The version gate stops there: nothing else is reported for a rule this build cannot read.
			if got := CheckCustomRule(cfg, mode); len(got) != 1 {
				t.Errorf("%s mode %d: problems %v, want only %s", c.json, mode, reasons(got), c.reason)
			}
		}
		if c.reason != "" {
			if _, err := NewCustomRule(cfg); err == nil {
				t.Errorf("%s: NewCustomRule compiled a rule this build cannot read", c.json)
			}
		}
	}
	for _, bad := range []string{`{"id":"r","kind":7}`, `{"id":"r","schemaVersion":"1"}`, `{"id":"r","schemaVersion":1.5}`} {
		if _, err := DecodeCustomRule([]byte(bad)); err == nil {
			t.Errorf("%s decoded; want INVALID_JSON", bad)
		}
	}
}

// C10: the checks NewCustomRule always made keep their messages, now with a reason and field.
func TestCustomRule_ValidationReasonsKeepTheirMessages(t *testing.T) {
	cases := []struct {
		cfg    CustomRuleConfig
		reason string
		field  string
		msg    string
	}{
		{CustomRuleConfig{ID: "org-rule", Severity: "critical", NameMatch: "x"}, ReasonInvalidSeverity, "severity",
			`custom rule "org-rule": invalid severity "critical" (must be one of [error warning info])`},
		{CustomRuleConfig{ID: "r", NameMatch: "*["}, ReasonInvalidPattern, "nameMatch",
			"error parsing regexp: missing argument to repetition operator: `*`"},
		{CustomRuleConfig{ID: "r", RawTypeMatch: "[invalid"}, ReasonInvalidPattern, "rawTypeMatch",
			"error parsing regexp: missing closing ]: `[invalid`"},
		{CustomRuleConfig{ID: "custom-bad", AutoFix: "bogus-fixer"}, ReasonUnknownAutoFix, "autoFix",
			`custom rule "custom-bad": unknown autoFix "bogus-fixer"`},
		// Several problems: the first in today's order wins, as before.
		{CustomRuleConfig{ID: "r", Severity: "high", NameMatch: "*[", AutoFix: "nope"}, ReasonInvalidSeverity, "severity",
			`custom rule "r": invalid severity "high" (must be one of [error warning info])`},
	}
	for _, c := range cases {
		for _, mode := range []CustomRuleMode{CustomRuleStrict, CustomRuleTolerant} {
			_, _, err := CompileCustomRule(c.cfg, mode)
			var ce *CustomRuleError
			if !errors.As(err, &ce) || ce.Reason != c.reason || ce.Field != c.field || err.Error() != c.msg ||
				ce.RuleID != c.cfg.ID || ce.Index != -1 {
				t.Errorf("%+v mode %d: err %#v, want %s on %s: %s", c.cfg, mode, err, c.reason, c.field, c.msg)
			}
		}
		if _, err := NewCustomRule(c.cfg); err == nil || err.Error() != c.msg {
			t.Errorf("NewCustomRule(%+v) = %v, want %s", c.cfg, err, c.msg)
		}
	}
	_, _, err := CompileCustomRule(CustomRuleConfig{ID: "r", RawTypeMatch: "[x"}, CustomRuleStrict)
	var se *syntax.Error
	if !errors.As(err, &se) {
		t.Errorf("pattern error does not unwrap to the regexp error: %#v", err)
	}
	all := CheckCustomRule(CustomRuleConfig{ID: "r", Severity: "high", NameMatch: "*[", RawTypeMatch: "[", AutoFix: "nope"}, CustomRuleStrict)
	if got, want := reasons(all), []string{ReasonInvalidSeverity, ReasonInvalidPattern, ReasonInvalidPattern, ReasonUnknownAutoFix}; !slices.Equal(got, want) {
		t.Errorf("CheckCustomRule = %v, want %v", got, want)
	}
}

// C12 + C13 + RESERVED_ID: what Strict refuses and Tolerant keeps with a warning.
func TestCustomRule_IDsAndMatchers(t *testing.T) {
	builtin := AllRules()[0].ID()
	cases := []struct {
		name       string
		cfg        CustomRuleConfig
		strictErr  string   // "" = Strict compiles
		strictWarn []string // Strict warnings when it compiles
		tolWarn    []string
	}{
		{"missing id", CustomRuleConfig{NameMatch: "x"}, ReasonMissingID, nil, []string{ReasonMissingID}},
		{"blank id", CustomRuleConfig{ID: "  ", NameMatch: "x"}, ReasonMissingID, nil, []string{ReasonMissingID}},
		{"built-in id", CustomRuleConfig{ID: builtin, NameMatch: "x"}, ReasonReservedID, nil, []string{ReasonReservedID}},
		{"no matcher", CustomRuleConfig{ID: "valid-rule", Name: "Valid", Severity: "warning"}, "", []string{ReasonNoMatcher}, nil},
		{"empty maps are no matcher", CustomRuleConfig{ID: "r", PropertyHas: map[string]string{}}, "", []string{ReasonNoMatcher}, nil},
		{"plain", CustomRuleConfig{ID: "r", PropertyNot: map[string]string{"k": "v"}}, "", nil, nil},
	}
	for _, c := range cases {
		r, w, err := CompileCustomRule(c.cfg, CustomRuleStrict)
		var ce *CustomRuleError
		switch {
		case c.strictErr != "" && (r != nil || !errors.As(err, &ce) || ce.Reason != c.strictErr || ce.Field != "id"):
			t.Errorf("%s: strict = %v, %v; want %s on id", c.name, r, err, c.strictErr)
		case c.strictErr == "" && (err != nil || !slices.Equal(reasons(w), c.strictWarn)):
			t.Errorf("%s: strict = %v, warnings %v; want warnings %v", c.name, err, reasons(w), c.strictWarn)
		}
		r, w, err = CompileCustomRule(c.cfg, CustomRuleTolerant)
		if err != nil || r == nil || !slices.Equal(reasons(w), c.tolWarn) {
			t.Errorf("%s: tolerant = %v, warnings %v; want a rule with warnings %v", c.name, err, reasons(w), c.tolWarn)
		}
	}
	for _, b := range AllRules() {
		if _, _, err := CompileCustomRule(CustomRuleConfig{ID: b.ID(), NameMatch: "x"}, CustomRuleStrict); err == nil {
			t.Errorf("strict accepted the built-in id %q", b.ID())
		}
	}
}

// MatchDigest moves with what decides matching and the fix, and with nothing else.
func TestCustomRule_MatchDigest(t *testing.T) {
	base := CustomRuleConfig{ID: "r", Name: "n", Description: "d", Severity: "warning", Category: "Style",
		Suggestion: "s", TypeMatch: "Action", RawTypeMatch: "^SET$", NameMatch: "x",
		PropertyHas: map[string]string{"k": "v"}, PropertyNot: map[string]string{"j": "w"}, AutoFix: "remove-block"}
	digests := func(c CustomRuleConfig) (string, string) {
		r, _ := mustCompile(t, c, CustomRuleTolerant)
		return r.MatchDigest(), r.RuleDigest()
	}
	baseMatch, baseRule := digests(base)
	same := map[string]func(*CustomRuleConfig){
		"id":            func(c *CustomRuleConfig) { c.ID = "other" },
		"name":          func(c *CustomRuleConfig) { c.Name = "other" },
		"description":   func(c *CustomRuleConfig) { c.Description = "other" },
		"severity":      func(c *CustomRuleConfig) { c.Severity = "error" },
		"category":      func(c *CustomRuleConfig) { c.Category = "Security" },
		"suggestion":    func(c *CustomRuleConfig) { c.Suggestion = "other" },
		"kind spelled":  func(c *CustomRuleConfig) { c.Kind = CustomRuleKindBlock },
		"version 1":     func(c *CustomRuleConfig) { c.SchemaVersion = 1 },
		"unknown field": func(c *CustomRuleConfig) { c.unknown = map[string]json.RawMessage{"x": json.RawMessage("1")} },
	}
	moved := map[string]func(*CustomRuleConfig){
		"typeMatch":    func(c *CustomRuleConfig) { c.TypeMatch = "Variable" },
		"rawTypeMatch": func(c *CustomRuleConfig) { c.RawTypeMatch = "^GET$" },
		"nameMatch":    func(c *CustomRuleConfig) { c.NameMatch = "y" },
		"propertyHas":  func(c *CustomRuleConfig) { c.PropertyHas = map[string]string{"k": "v2"} },
		"propertyNot":  func(c *CustomRuleConfig) { c.PropertyNot = nil },
		"autoFix":      func(c *CustomRuleConfig) { c.AutoFix = "" },
	}
	for name, edit := range same {
		c := base
		edit(&c)
		if m, _ := digests(c); m != baseMatch {
			t.Errorf("editing %s moved MatchDigest: a cosmetic edit would make the rule new", name)
		}
	}
	for name, edit := range moved {
		c := base
		edit(&c)
		if m, _ := digests(c); m == baseMatch {
			t.Errorf("editing %s kept MatchDigest: a match edit must make the rule new", name)
		}
	}
	// RuleDigest stays the full-config identity the caches need.
	c := base
	c.Name = "other"
	if _, rd := digests(c); rd == baseRule {
		t.Error("RuleDigest ignores the name; two orgs' rules under one id would share cached findings")
	}
	// Empty and absent maps match the same blocks, so they digest the same.
	c = base
	c.PropertyNot = map[string]string{}
	d1, _ := digests(c)
	c.PropertyNot = nil
	if d2, _ := digests(c); d1 != d2 {
		t.Error("an empty propertyNot and a missing one have different MatchDigests")
	}
}

// C11: what this release writes reads back unchanged in a release that knows neither kind nor schemaVersion.
func TestCustomRule_WriteEncodingRollsBack(t *testing.T) {
	// Nine unknown keys, stored in reverse order: more than a small map holds, so an unsorted write would almost
	// never come out sorted, and every round decodes fresh maps.
	const stored = `{"id":"r","zz":0,"when":{"a":[1, 2]},"stage":"preview","s9":9,"s8":8,"s7":7,"s6":6,"s5":5,"aa":true}`
	const wantTail = `"aa":true,"s5":5,"s6":6,"s7":7,"s8":8,"s9":9,"stage":"preview","when":{"a":[1,2]},"zz":0}`
	var cfg CustomRuleConfig
	var encoded []byte
	for range 16 {
		cfg = CustomRuleConfig{ID: "r", Name: "n", Severity: "info", TypeMatch: "Action",
			PropertyHas: map[string]string{"a": "b"}, AutoFix: "remove-block"}
		cfg = cfg.WithUnknownFieldsFrom(decodeOne(t, stored))
		var err error
		if encoded, err = EncodeCustomRule(cfg); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(encoded, []byte(wantTail)) {
			t.Fatalf("write encoding = %s, want the unknown fields last, sorted and compact", encoded)
		}
	}
	if !json.Valid(encoded) {
		t.Fatalf("write encoding is not JSON: %s", encoded)
	}
	var old customRuleConfigV0
	if err := json.Unmarshal(encoded, &old); err != nil {
		t.Fatal(err)
	}
	want := customRuleConfigV0{ID: "r", Name: "n", Severity: "info", TypeMatch: "Action",
		PropertyHas: map[string]string{"a": "b"}, AutoFix: "remove-block"}
	if !reflect.DeepEqual(old, want) {
		t.Errorf("previous release reads %+v, want %+v", old, want)
	}
	back := decodeOne(t, string(encoded))
	if got, want := back.UnknownFields(), decodeOne(t, stored).UnknownFields(); !slices.Equal(got, want) {
		t.Errorf("unknown fields after a save = %v, want the stored row's %v", got, want)
	}
	r1, _ := mustCompile(t, cfg, CustomRuleTolerant)
	r2, _ := mustCompile(t, back, CustomRuleTolerant)
	if r1.RuleDigest() != r2.RuleDigest() || r1.MatchDigest() != r2.MatchDigest() {
		t.Error("a write and read-back moved a digest")
	}
	// The write encoding never adds a key the struct does not know when there is nothing unknown.
	plain, _ := EncodeCustomRule(CustomRuleConfig{ID: "r"})
	if want := `{"kind":"block","schemaVersion":1,"id":"r","name":"","description":"","severity":"","category":"",` +
		`"rawTypeMatch":"","nameMatch":"","propertyHas":null,"propertyNot":null,"typeMatch":"","suggestion":""}`; string(plain) != want {
		t.Errorf("write encoding = %s, want %s", plain, want)
	}
}

// The save merge (D8): the edit keeps its own unknown fields and wins on a shared key, the stored row's other unknown
// fields survive, and neither input changes.
func TestCustomRule_WithUnknownFieldsFrom(t *testing.T) {
	c := decodeOne(t, `{"id":"r","nameMatch":"x","stage":"stable","mine":true}`)
	stored := decodeOne(t, `{"id":"r","nameMatch":"old","stage":"preview","when":{"a":1}}`)
	merged := c.WithUnknownFieldsFrom(stored)
	if got, want := merged.UnknownFields(), []string{"mine", "stage", "when"}; !slices.Equal(got, want) {
		t.Errorf("merged unknown fields = %v, want %v", got, want)
	}
	for k, want := range map[string]string{"mine": `true`, "stage": `"stable"`, "when": `{"a":1}`} {
		if got := string(merged.unknown[k]); got != want {
			t.Errorf("merged %s = %s, want %s", k, got, want)
		}
	}
	if merged.NameMatch != "x" {
		t.Errorf("merged nameMatch = %q, want the edit's", merged.NameMatch)
	}
	if got := string(c.unknown["stage"]) + string(stored.unknown["stage"]); got != `"stable""preview"` || len(c.unknown) != 2 {
		t.Errorf("the merge changed its inputs: c %v, stored %v", c.unknown, stored.unknown)
	}
	if got := c.WithUnknownFieldsFrom(CustomRuleConfig{}).UnknownFields(); !slices.Equal(got, []string{"mine", "stage"}) {
		t.Errorf("merging an empty stored row = %v, want the edit's own [mine stage]", got)
	}
}

// C9: the document shapes every reader accepts, each entry on its own.
func TestDecodeCustomRuleDocument(t *testing.T) {
	cases := []struct {
		doc string
		ids []string // nil with wantErr
		err string
	}{
		{`null`, []string{}, ""},
		{` [ ] `, []string{}, ""},
		{`[{"id":"a"},{"id":"b"}]`, []string{"a", "b"}, ""},
		{`[null]`, []string{""}, ""},
		{`{"id":"a","nameMatch":"x"}`, []string{"a"}, ""},
		{`{"rules":[{"id":"a"}]}`, []string{"a"}, ""},
		{`{"Rules":{"id":"a"}}`, []string{"a"}, ""},
		{`{"rules":null}`, []string{}, ""},
		{`{"rules":{"rules":[{"id":"a"}]}}`, nil, "not a custom-rule JSON array or object"},
		{`{}`, nil, "not a custom-rule JSON array or object"},
		{`{"name":"no id"}`, nil, "not a custom-rule JSON array or object"},
		{`{"id":""}`, nil, "not a custom-rule JSON array or object"},
		{`{"id":5}`, nil, "not a custom-rule JSON array or object"},
		{`"str"`, nil, "not a custom-rule JSON array or object"},
		{`42`, nil, "not a custom-rule JSON array or object"},
		{`{not valid json`, nil, "invalid character 'n' looking for beginning of object key string"},
		{``, nil, "unexpected end of JSON input"},
	}
	for _, c := range cases {
		entries, err := DecodeCustomRuleDocument([]byte(c.doc))
		if c.err != "" {
			var ce *CustomRuleError
			if !errors.As(err, &ce) || ce.Reason != ReasonInvalidJSON || err.Error() != c.err {
				t.Errorf("%s: err %v, want INVALID_JSON %q", c.doc, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.doc, err)
			continue
		}
		ids := []string{}
		for i, e := range entries {
			if e.Err != nil || e.Index != i {
				t.Errorf("%s: entry %d = %+v", c.doc, i, e)
			}
			ids = append(ids, e.Config.ID)
		}
		if !slices.Equal(ids, c.ids) {
			t.Errorf("%s: ids %v, want %v", c.doc, ids, c.ids)
		}
	}

	// C8: a wrong JSON type is that entry's error, with encoding/json's message, its field and the id it had.
	entries, err := DecodeCustomRuleDocument([]byte(`[{"id":"ok"},{"id":"bad","severity":3},"x",{"id":"also-ok"}]`))
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries %v, err %v", entries, err)
	}
	if entries[0].Err != nil || entries[3].Err != nil || entries[3].Config.ID != "also-ok" {
		t.Errorf("good entries = %+v / %+v", entries[0], entries[3])
	}
	bad := entries[1].Err
	if bad == nil || bad.Index != 1 || bad.RuleID != "bad" || bad.Field != "severity" || bad.Reason != ReasonInvalidJSON ||
		bad.Error() != "json: cannot unmarshal number into Go struct field CustomRuleConfig.severity of type string" {
		t.Errorf("type-error entry = %+v", bad)
	}
	if e := entries[2].Err; e == nil || e.Error() != "json: cannot unmarshal string into Go value of type analyzer.CustomRuleConfig" {
		t.Errorf("non-object entry = %+v", e)
	}
	var te *json.UnmarshalTypeError
	if !errors.As(bad, &te) {
		t.Error("an INVALID_JSON entry error does not unwrap to the encoding/json error")
	}
}

func writeCustomRules(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The file loaders: tolerant, typed problems, today's whole-file failure on a type error.
func TestLoadCustomRuleFile(t *testing.T) {
	path := writeCustomRules(t, `[
		{"id":"ok","rawTypeMatch":"^SET$","nameMatches":"typo"},
		{"kind":"flow","schemaVersion":2,"id":"needs-retry","when":{"lacks":"Retry"}},
		{"id":"bad-sev","severity":"critical","nameMatch":"x"},
		{"rawTypeMatch":"x"},
		{"id":"`+AllRules()[0].ID()+`","nameMatch":"x"}
	]`)
	rules, problems, err := LoadCustomRuleFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rules {
		ids = append(ids, r.ID())
	}
	if want := []string{"ok", "", AllRules()[0].ID()}; !slices.Equal(ids, want) {
		t.Errorf("rules %v, want %v (unknown kind and bad severity skipped, the rest kept)", ids, want)
	}
	type got struct {
		index   int
		reason  string
		warning bool
	}
	var gots []got
	for _, p := range problems {
		gots = append(gots, got{p.Index, p.Reason, p.Warning})
	}
	want := []got{{0, ReasonUnknownField, true}, {1, ReasonUnknownKind, false}, {2, ReasonInvalidSeverity, false},
		{3, ReasonMissingID, true}, {4, ReasonReservedID, true}}
	if !slices.Equal(gots, want) {
		t.Errorf("problems %+v, want %+v", gots, want)
	}

	_, warnings, err := LoadCustomRules(path)
	if err != nil || len(warnings) != 5 {
		t.Fatalf("LoadCustomRules: %v, %d warnings %v", err, len(warnings), warnings)
	}
	for i, w := range []string{
		`rules.json: entry 0 (rule "ok"): custom rule "ok": unknown field "nameMatches" ignored`,
		`rules.json: entry 1 (rule "needs-retry") skipped: custom rule "needs-retry": unknown kind "flow"`,
		`rules.json: entry 2 (rule "bad-sev") skipped: custom rule "bad-sev": invalid severity "critical"`,
		`rules.json: entry 3 (rule "#3 (no id)"): custom rule has no id`,
	} {
		if !strings.HasPrefix(warnings[i], w) {
			t.Errorf("warning %d = %q, want prefix %q", i, warnings[i], w)
		}
	}

	if _, _, err := LoadCustomRuleFile(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v, want fs.ErrNotExist so callers can refuse a named file", err)
	}
}

// C8: a type error in any entry still fails the whole file, with the message one json.Unmarshal of the array gave.
func TestLoadCustomRules_TypeErrorFailsTheFile(t *testing.T) {
	for _, body := range []string{
		`[{"id":"a"},{"id":"b","severity":3,"name":4},{"propertyHas":"x"}]`,
		`[{"id":"a"},"str"]`,
		`[{"id":"a","propertyNot":{"k":[1]}}]`,
		`[{"id":"a"},{"id":"b"]`,
	} {
		var legacy []customRuleConfigFields
		want := strings.ReplaceAll(json.Unmarshal([]byte(body), &legacy).Error(), "customRuleConfigFields", "CustomRuleConfig")
		rules, _, err := LoadCustomRules(writeCustomRules(t, body))
		if err == nil || rules != nil || err.Error() != want {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
}

// A newer release's rule may give a field another JSON type (ADR 0015): its
// decode error is the newer kind or version, so a rules file skips it as a
// problem (bakicli: "needs a newer bakicli") instead of failing on a type
// error. A negative schemaVersion is not newer: still INVALID_JSON.
func TestDecodeCustomRuleDocument_NewerReleaseRetypesAField(t *testing.T) {
	entries, err := DecodeCustomRuleDocument([]byte(`[
		{"id":"ok","rawTypeMatch":"^SET$"},
		{"kind":"block","schemaVersion":2,"id":"v2","nameMatch":["^Set","^Get"]},
		{"kind":"flow","id":"f","severity":{"level":"error"}},
		{"schemaVersion":-1,"id":"neg","nameMatch":["^Set"]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", ReasonUnsupportedVersion, ReasonUnknownKind, ReasonInvalidJSON}
	for i, e := range entries {
		got := ""
		if e.Err != nil {
			got = e.Err.Reason
			if e.Err.Index != i {
				t.Errorf("entry %d: error index %d", i, e.Err.Index)
			}
		}
		if got != want[i] {
			t.Errorf("entry %d: reason %q, want %q (%v)", i, got, want[i], e.Err)
		}
		if newer := e.Err.IsNewerRelease(); newer != (i == 1 || i == 2) {
			t.Errorf("entry %d: IsNewerRelease %t", i, newer)
		}
	}
	if entries[1].Err.RuleID != "v2" || entries[2].Err.RuleID != "f" {
		t.Errorf("the rule ids are kept: %q, %q", entries[1].Err.RuleID, entries[2].Err.RuleID)
	}

	rules, problems, err := LoadCustomRuleFile(writeCustomRules(t,
		`[{"id":"ok","rawTypeMatch":"^SET$"},{"kind":"flow","id":"f","severity":{"level":"error"}}]`))
	if err != nil || len(rules) != 1 || len(problems) != 1 || problems[0].Reason != ReasonUnknownKind {
		t.Errorf("rules file: %d rules, problems %v, err %v; want the newer rule skipped as a problem", len(rules), problems, err)
	}
}
