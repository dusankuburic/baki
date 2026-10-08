package settingsdoc

import (
	"encoding/json"
	"strings"
	"testing"
)

type testAppearance struct {
	Theme   string `json:"theme"`
	Density string `json:"density"`
	Note    string `json:"note,omitempty"`
}

type testProvider struct {
	Model string `json:"model"`
}

type testAI struct {
	Providers map[string]testProvider `json:"providers"`
	Budget    float64                 `json:"budget"`
	Prompts   []string                `json:"prompts"`
	Options   map[string]any          `json:"options,omitempty"`
}

type testDoc struct {
	Version    int            `json:"version"`
	Appearance testAppearance `json:"appearance"`
	AI         testAI         `json:"ai"`
}

func registry(t *testing.T, sections ...Section) *Registry {
	t.Helper()
	r, err := NewRegistry(sections...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// renameDense is an appearance upgrade to version 2: the boolean "dense" became the "density" string.
func renameDense(data json.RawMessage) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	if dense, ok := m["dense"].(bool); ok {
		if dense {
			m["density"] = "compact"
		} else {
			m["density"] = "comfortable"
		}
		delete(m, "dense")
	}
	return json.Marshal(m)
}

func assertJSON(t *testing.T, what string, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v in %s", what, err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: bad want: %v", what, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("%s:\n got  %s\n want %s", what, gb, wb)
	}
}

func TestNewRegistry_ChecksDeclarations(t *testing.T) {
	up := func(d json.RawMessage) (json.RawMessage, error) { return d, nil }
	for name, sections := range map[string][]Section{
		"empty key":          {{Key: "", Version: 1}},
		"the document key":   {{Key: DocVersionKey, Version: 1}},
		"declared twice":     {{Key: "a", Version: 1}, {Key: "a", Version: 1}},
		"version zero":       {{Key: "a", Version: 0}},
		"a step missing":     {{Key: "a", Version: 3, Upgrades: map[int]Upgrade{2: up}}},
		"a step beyond":      {{Key: "a", Version: 2, Upgrades: map[int]Upgrade{2: up, 3: up}}},
		"a step to legacy":   {{Key: "a", Version: 1, Upgrades: map[int]Upgrade{1: up}}},
		"a nil upgrade step": {{Key: "a", Version: 2, Upgrades: map[int]Upgrade{2: nil}}},
	} {
		if _, err := NewRegistry(sections...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewRegistry(Section{Key: "a", Version: 3, Upgrades: map[int]Upgrade{2: up, 3: up}}); err != nil {
		t.Errorf("a complete chain was refused: %v", err)
	}
}

// TestUpgrade_RunsEachStepFromTheStoredVersion: data stored at an older version goes through every later step in
// order; data at the current version or a newer one is left alone.
func TestUpgrade_RunsEachStepFromTheStoredVersion(t *testing.T) {
	var ran []string
	step := func(name string) Upgrade {
		return func(d json.RawMessage) (json.RawMessage, error) {
			ran = append(ran, name)
			return json.RawMessage(strings.TrimSuffix(string(d), "}") + `,"` + name + `":true}`), nil
		}
	}
	r := registry(t, Section{Key: "appearance", Version: 3, Upgrades: map[int]Upgrade{2: step("v2"), 3: step("v3")}})

	for _, tc := range []struct {
		from  int
		steps string
		want  int
	}{
		{1, "v2,v3", 3},
		{2, "v3", 3},
		{3, "", 3},
		{7, "", 7},
	} {
		ran = nil
		got, err := r.Upgrade("appearance", Stored{Version: tc.from, Data: json.RawMessage(`{"theme":"a"}`)})
		if err != nil {
			t.Fatalf("from %d: %v", tc.from, err)
		}
		if strings.Join(ran, ",") != tc.steps || got.Version != tc.want {
			t.Errorf("from %d: ran %v to version %d, want %q to %d", tc.from, ran, got.Version, tc.steps, tc.want)
		}
	}
}

func TestDecode_UpgradesOlderSections(t *testing.T) {
	r := registry(t, Section{Key: "appearance", Version: 2, Upgrades: map[int]Upgrade{2: renameDense}})
	var doc testDoc
	stored, err := r.Decode(map[string]Stored{
		"appearance": {Version: 1, Data: json.RawMessage(`{"theme":"light","dense":true}`)},
	}, &doc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if doc.Appearance.Theme != "light" || doc.Appearance.Density != "compact" {
		t.Errorf("appearance = %+v, want light and compact", doc.Appearance)
	}
	if stored["appearance"].Version != 2 {
		t.Errorf("Decode returned the section at version %d, want it upgraded to 2", stored["appearance"].Version)
	}
}

// TestDecode_ReplacesPresentSectionsAndKeepsAbsentOnes: a present section replaces the field (a map is not merged
// into the caller's defaults); an absent one leaves the caller's value.
func TestDecode_ReplacesPresentSectionsAndKeepsAbsentOnes(t *testing.T) {
	doc := testDoc{
		Appearance: testAppearance{Theme: "default"},
		AI:         testAI{Providers: map[string]testProvider{"a": {Model: "m"}}, Budget: 5},
	}
	if _, err := Default.Decode(map[string]Stored{
		"ai":      {Version: 1, Data: json.RawMessage(`{"providers":{"b":{"model":"n"}}}`)},
		"unknown": {Version: 1, Data: json.RawMessage(`{"x":1}`)},
	}, &doc); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := doc.AI.Providers["a"]; ok || doc.AI.Providers["b"].Model != "n" || doc.AI.Budget != 0 {
		t.Errorf("ai = %+v, want the stored section alone", doc.AI)
	}
	if doc.Appearance.Theme != "default" {
		t.Errorf("an absent section changed the field: %+v", doc.Appearance)
	}
}

// TestDecode_NewerSectionReadsTheFieldsItKnows: a newer binary may have changed a field's type; the section still
// reads, that field at its zero value. At this binary's own version the same data is an error.
func TestDecode_NewerSectionReadsTheFieldsItKnows(t *testing.T) {
	data := json.RawMessage(`{"theme":{"name":"dark"},"density":"compact","future":1}`)
	var doc testDoc
	if _, err := Default.Decode(map[string]Stored{"appearance": {Version: 9, Data: data}}, &doc); err != nil {
		t.Fatalf("Decode of a newer section: %v", err)
	}
	if doc.Appearance.Density != "compact" || doc.Appearance.Theme != "" {
		t.Errorf("appearance = %+v, want density compact and no theme", doc.Appearance)
	}
	if _, err := Default.Decode(map[string]Stored{"appearance": {Version: 1, Data: data}}, &testDoc{}); err == nil {
		t.Error("a section at the current version with a mistyped field decoded without an error")
	}
}

// TestEncode_UnchangedLayoutWithoutUnknownFields: with nothing to carry over, a section stores exactly what
// encoding/json writes for its field, on the first write and on every later one.
func TestEncode_UnchangedLayoutWithoutUnknownFields(t *testing.T) {
	doc := testDoc{
		Version:    1,
		Appearance: testAppearance{Theme: "dark", Density: "compact", Note: "<&>"},
		AI: testAI{
			Providers: map[string]testProvider{"z": {Model: "1"}, "a": {Model: "2"}},
			Budget:    2.5, Prompts: []string{"p"}, Options: map[string]any{"k": []any{1.5, "x"}},
		},
	}
	first, err := Default.Encode(doc, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	second, err := Default.Encode(&doc, first)
	if err != nil {
		t.Fatalf("Encode over the first: %v", err)
	}
	for key, value := range map[string]any{"appearance": doc.Appearance, "ai": doc.AI} {
		want, _ := json.Marshal(value)
		for i, got := range []map[string]Stored{first, second} {
			if string(got[key].Data) != string(want) || got[key].Version != LegacyVersion {
				t.Errorf("write %d, %s = v%d %s, want v1 %s", i+1, key, got[key].Version, got[key].Data, want)
			}
		}
	}
	if _, ok := first[DocVersionKey]; ok {
		t.Error("the document's version was stored as a section")
	}
}

// TestEncode_KeepsWhatThisBinaryDoesNotKnow: fields a newer binary stored survive a write at every depth (a
// struct, a map entry's struct), while what this binary knows follows the write: a removed map entry and a
// cleared omitempty field stay gone.
func TestEncode_KeepsWhatThisBinaryDoesNotKnow(t *testing.T) {
	stored := map[string]Stored{
		"ai": {Version: 1, Data: json.RawMessage(`{"providers":{"claude":{"model":"x","topP":0.9},"gone":{"model":"y"}},` +
			`"budget":5,"prompts":["a"],"futureKnob":true,"options":{"old":1}}`)},
		"appearance": {Version: 1, Data: json.RawMessage(`{"theme":"a","density":"b","note":"n","Theme":"stale","future":[1]}`)},
		"notInDoc":   {Version: 4, Data: json.RawMessage(`{"x":1}`)},
	}
	doc := testDoc{
		Appearance: testAppearance{Theme: "c", Density: "d"},
		AI:         testAI{Providers: map[string]testProvider{"claude": {Model: "z"}}, Budget: 7, Prompts: []string{"b"}},
	}
	got, err := Default.Encode(doc, stored)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	assertJSON(t, "ai", got["ai"].Data, `{"providers":{"claude":{"model":"z","topP":0.9}},"budget":7,"prompts":["b"],"futureKnob":true}`)
	assertJSON(t, "appearance", got["appearance"].Data, `{"theme":"c","density":"d","future":[1]}`)
	if _, ok := got["notInDoc"]; ok {
		t.Error("a stored section the document lacks was returned")
	}
}

// TestEncode_NeverDowngradesANewerSection: a section a newer binary stored keeps its version, its unknown fields,
// and a known field whose type that version changed; the fields this binary can edit follow the write.
func TestEncode_NeverDowngradesANewerSection(t *testing.T) {
	stored := map[string]Stored{"appearance": {Version: 5, Data: json.RawMessage(`{"theme":{"name":"x"},"density":"b","future":1}`)}}
	got, err := Default.Encode(testDoc{Appearance: testAppearance{Theme: "dark", Density: "c"}}, stored)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got["appearance"].Version != 5 {
		t.Errorf("version = %d, want 5 kept", got["appearance"].Version)
	}
	assertJSON(t, "appearance", got["appearance"].Data, `{"theme":{"name":"x"},"density":"c","future":1}`)

	// At this binary's own version a value that does not decode is overwritten: there is no newer reader to keep it for.
	stored["appearance"] = Stored{Version: 1, Data: json.RawMessage(`{"theme":{"name":"x"},"density":"b"}`)}
	got, err = Default.Encode(testDoc{Appearance: testAppearance{Theme: "dark", Density: "c"}}, stored)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	assertJSON(t, "appearance at the current version", got["appearance"].Data, `{"theme":"dark","density":"c"}`)
}

// TestEncode_UpgradesAnOlderSectionBeforeMerging: the stored side of the merge is brought to this binary's version
// first, so a field the upgrade renamed is not carried over under its old name.
func TestEncode_UpgradesAnOlderSectionBeforeMerging(t *testing.T) {
	r := registry(t, Section{Key: "appearance", Version: 2, Upgrades: map[int]Upgrade{2: renameDense}})
	stored := map[string]Stored{"appearance": {Version: 1, Data: json.RawMessage(`{"theme":"a","dense":true,"future":1}`)}}
	got, err := r.Encode(testDoc{Appearance: testAppearance{Theme: "b", Density: "comfortable"}}, stored)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got["appearance"].Version != 2 {
		t.Errorf("version = %d, want 2", got["appearance"].Version)
	}
	assertJSON(t, "appearance", got["appearance"].Data, `{"theme":"b","density":"comfortable","future":1}`)
	if got["ai"].Version != LegacyVersion {
		t.Errorf("an undeclared section got version %d, want %d", got["ai"].Version, LegacyVersion)
	}
}

// selfEncoded has its own JSON encoding, an object whose keys are not its fields.
type selfEncoded struct{ n int }

func (s selfEncoded) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]int{"value": s.n}) }

func (s *selfEncoded) UnmarshalJSON(data []byte) error {
	var m map[string]int
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	s.n = m["value"]
	return nil
}

// TestEncode_SelfEncodingTypesAreValues: a type with its own JSON encoding is written whole, never merged by its
// fields (it has none that match its keys).
func TestEncode_SelfEncodingTypesAreValues(t *testing.T) {
	type section struct {
		V selfEncoded `json:"v"`
		X int         `json:"x"`
	}
	type doc struct {
		S section `json:"s"`
	}
	got, err := Default.Encode(doc{S: section{V: selfEncoded{n: 3}, X: 4}},
		map[string]Stored{"s": {Version: 1, Data: json.RawMessage(`{"v":{"value":1,"extra":2},"x":1,"y":2}`)}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	assertJSON(t, "s", got["s"].Data, `{"v":{"value":3},"x":4,"y":2}`)
}
