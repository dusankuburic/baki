package analyzer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// CustomRuleKind names what a custom rule matches and at which scope.
type CustomRuleKind string

// CustomRuleKindBlock matches one block at a time (typeMatch, rawTypeMatch, nameMatch, propertyHas, propertyNot):
// every custom rule written before kinds existed.
const CustomRuleKindBlock CustomRuleKind = "block"

// CustomRuleSchemaVersion is the newest custom-rule schema this build reads and writes. A missing schemaVersion
// means 1, forever.
const CustomRuleSchemaVersion = 1

// customRuleKinds maps each kind this build runs to the newest schemaVersion it reads for it.
var customRuleKinds = map[CustomRuleKind]int{CustomRuleKindBlock: 1}

// CustomRuleMode says how a custom rule is checked (ADR 0015).
type CustomRuleMode uint8

const (
	// CustomRuleStrict is for authoring and writes (API save, validate and test, bakicli rules test and lint):
	// an unknown field, a missing id and a built-in rule's id are errors. The zero mode counts as Strict.
	CustomRuleStrict CustomRuleMode = iota + 1
	// CustomRuleTolerant is for stored rows and checked-in files (PAD_CUSTOM_RULES, -custom-rules, .bakirc.json):
	// those are warnings and the rule still runs.
	CustomRuleTolerant
)

// Stable reason codes of a CustomRuleError; the API's details.reason and bakicli's JSON output carry them.
const (
	ReasonInvalidJSON        = "INVALID_JSON"
	ReasonUnknownField       = "UNKNOWN_FIELD"
	ReasonUnknownKind        = "UNKNOWN_KIND"
	ReasonUnsupportedVersion = "UNSUPPORTED_VERSION"
	ReasonMissingID          = "MISSING_ID"
	ReasonReservedID         = "RESERVED_ID"
	ReasonInvalidSeverity    = "INVALID_SEVERITY"
	ReasonInvalidPattern     = "INVALID_PATTERN"
	ReasonUnknownAutoFix     = "UNKNOWN_AUTOFIX"
	// ReasonNoMatcher is only ever a warning (Strict mode): such a rule is valid and matches every block.
	ReasonNoMatcher = "NO_MATCHER"
)

// CustomRuleError is one problem with one custom rule: an error, or a warning when the mode tolerates it.
type CustomRuleError struct {
	Index   int    // the entry's position in its document; -1 for a lone rule
	RuleID  string // the rule's id as decoded; "" when it has none
	Field   string // JSON name: "kind", "nameMatch", the unknown key; "" for the rule as a whole
	Reason  string // one of the Reason* codes
	Msg     string // the message; the wording NewCustomRule always used where one existed
	Warning bool   // the mode tolerates it and the rule still runs
	Err     error  // the cause (a regexp or encoding/json error), if any
}

func (e *CustomRuleError) Error() string {
	if e.Msg == "" && e.Err != nil {
		return e.Err.Error()
	}
	return e.Msg
}

func (e *CustomRuleError) Unwrap() error { return e.Err }

// customRuleConfigFields is CustomRuleConfig without its methods, so UnmarshalJSON can hand the known fields to
// encoding/json.
type customRuleConfigFields CustomRuleConfig

var (
	customRuleConfigType = reflect.TypeFor[CustomRuleConfig]()
	customRuleFieldsType = reflect.TypeFor[customRuleConfigFields]()
	// customRuleKeys is the JSON name of every exported field, so a field added later is known automatically.
	customRuleKeys = jsonFieldNames(customRuleConfigType)
)

func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}

// isKnownCustomRuleKey matches a key to a field the way encoding/json does: exactly, else case-insensitively
// (bytes.EqualFold, which is what its folded-name lookup implements).
func isKnownCustomRuleKey(key string) bool {
	for _, name := range customRuleKeys {
		if key == name || strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// UnmarshalJSON decodes the known fields exactly as encoding/json does without this method: keys match
// case-insensitively, null is a no-op, a known field's type error has the same message, and decoding into a
// filled value merges. It also records the top-level keys this build does not know (UnknownFields).
//
// Inside another struct (a request body, .bakirc.json) a type error names that struct, not CustomRuleConfig, before
// the same field path: encoding/json rewrites the struct name of every json.Unmarshaler's type error.
func (c *CustomRuleConfig) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	c.unknown = nil
	if err := json.Unmarshal(b, (*customRuleConfigFields)(c)); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			if te.Struct == customRuleFieldsType.Name() {
				te.Struct = customRuleConfigType.Name()
			}
			if te.Type == customRuleFieldsType {
				te.Type = customRuleConfigType
			}
		}
		return err
	}
	var all map[string]json.RawMessage
	_ = json.Unmarshal(b, &all) // b is an object: only an object (or null) decodes into the struct
	for key, raw := range all {
		if isKnownCustomRuleKey(key) {
			continue
		}
		if c.unknown == nil {
			c.unknown = make(map[string]json.RawMessage)
		}
		c.unknown[key] = raw
	}
	return nil
}

// UnknownFields returns the top-level keys of the decoded JSON that this build does not know, sorted.
func (c CustomRuleConfig) UnknownFields() []string {
	if len(c.unknown) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.unknown))
	for k := range c.unknown {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Normalized returns c with kind and schemaVersion spelled out: a missing kind is block, a missing version 1.
func (c CustomRuleConfig) Normalized() CustomRuleConfig {
	if c.Kind == "" {
		c.Kind = CustomRuleKindBlock
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = 1
	}
	return c
}

// WithUnknownFieldsFrom returns c also carrying stored's unknown fields that c lacks, so a save keeps the fields a
// newer release added to the stored row (forward compatibility, as settings sections do).
func (c CustomRuleConfig) WithUnknownFieldsFrom(stored CustomRuleConfig) CustomRuleConfig {
	if len(stored.unknown) == 0 {
		return c
	}
	merged := make(map[string]json.RawMessage, len(c.unknown)+len(stored.unknown))
	for k, v := range stored.unknown {
		merged[k] = v
	}
	for k, v := range c.unknown {
		merged[k] = v
	}
	c.unknown = merged
	return c
}

// EncodeCustomRule is the write encoding: the Normalized known fields, kind and schemaVersion first, then the unknown
// fields c carries, sorted and verbatim. Reading it back gives the same RuleDigest and MatchDigest as c.
func EncodeCustomRule(c CustomRuleConfig) ([]byte, error) {
	known, err := json.Marshal(c.Normalized())
	if err != nil || len(c.unknown) == 0 {
		return known, err
	}
	var buf bytes.Buffer
	buf.Write(known[:len(known)-1])
	sep := len(known) > 2
	for _, k := range c.UnknownFields() {
		if sep {
			buf.WriteByte(',')
		}
		sep = true
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		if err := json.Compact(&buf, c.unknown[k]); err != nil {
			return nil, fmt.Errorf("custom rule %q: field %q: %w", c.ID, k, err)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// DecodeCustomRule decodes one stored rule (an org row's config). A JSON error is an INVALID_JSON *CustomRuleError
// carrying encoding/json's message.
func DecodeCustomRule(raw []byte) (CustomRuleConfig, error) {
	e := decodeCustomRuleEntry(-1, raw)
	if e.Err != nil {
		return CustomRuleConfig{}, e.Err
	}
	return e.Config, nil
}

// PartialCustomRule returns the fields of raw that decode, for showing a stored rule that does not (INVALID_JSON)
// as it was: encoding/json fills every field but a mistyped one. Unknown fields are not kept; never run or save it
// as the rule.
func PartialCustomRule(raw []byte) CustomRuleConfig {
	var cfg CustomRuleConfig
	_ = json.Unmarshal(raw, &cfg)
	return cfg
}

// CustomRuleEntry is one entry of a rules document: its config, or the JSON error that makes it unreadable.
type CustomRuleEntry struct {
	Index  int
	Config CustomRuleConfig // zero when Err is set
	// Err is INVALID_JSON, with the rule's id when it decoded; or, for a newer release's rule whose fields do not
	// decode, UNKNOWN_KIND or UNSUPPORTED_VERSION (IsNewerRelease).
	Err *CustomRuleError
}

func notARulesDocument() *CustomRuleError {
	return &CustomRuleError{Index: -1, Reason: ReasonInvalidJSON, Msg: "not a custom-rule JSON array or object"}
}

// DecodeCustomRuleDocument reads a rules document: an array of rules, one rule object with a non-empty id, or either
// inside {"rules": …}; null is an empty document. Each entry decodes on its own, so a wrong JSON type is that entry's
// INVALID_JSON, not the document's. Malformed JSON or another shape is an INVALID_JSON *CustomRuleError.
func DecodeCustomRuleDocument(data []byte) ([]CustomRuleEntry, error) {
	var doc json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &CustomRuleError{Index: -1, Reason: ReasonInvalidJSON, Msg: err.Error(), Err: err}
	}
	if doc[0] == '{' {
		var envelope struct {
			Rules json.RawMessage `json:"rules"`
		}
		if json.Unmarshal(doc, &envelope) == nil && len(envelope.Rules) > 0 {
			doc = envelope.Rules
		}
	}
	switch doc[0] {
	case 'n':
		return nil, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(doc, &items); err != nil {
			return nil, &CustomRuleError{Index: -1, Reason: ReasonInvalidJSON, Msg: err.Error(), Err: err}
		}
		entries := make([]CustomRuleEntry, len(items))
		for i, item := range items {
			entries[i] = decodeCustomRuleEntry(i, item)
		}
		return entries, nil
	case '{':
		// A lone object is a rule only when it names itself, as before; anything else is not a rules document.
		var named struct {
			ID json.RawMessage `json:"id"`
		}
		var id string
		if json.Unmarshal(doc, &named) != nil || json.Unmarshal(named.ID, &id) != nil || id == "" {
			return nil, notARulesDocument()
		}
		return []CustomRuleEntry{decodeCustomRuleEntry(0, doc)}, nil
	}
	return nil, notARulesDocument()
}

func decodeCustomRuleEntry(index int, raw []byte) CustomRuleEntry {
	var cfg CustomRuleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		if p := newerReleaseError(raw); p != nil {
			p.Index = index
			return CustomRuleEntry{Index: index, Err: p}
		}
		field := ""
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			field = te.Field
		}
		return CustomRuleEntry{Index: index, Err: &CustomRuleError{Index: index, RuleID: cfg.ID, Field: field,
			Reason: ReasonInvalidJSON, Msg: err.Error(), Err: err}}
	}
	return CustomRuleEntry{Index: index, Config: cfg}
}

// newerReleaseError is the UNKNOWN_KIND or UNSUPPORTED_VERSION of a rule whose fields do not decode: a later kind or
// schemaVersion may give a field another JSON type (ADR 0015), and such a rule needs a newer version rather than
// being invalid JSON. Nil when its kind and schemaVersion are this build's, or do not decode either; a negative
// schemaVersion is not a newer release's.
func newerReleaseError(raw []byte) *CustomRuleError {
	var head struct {
		ID            json.RawMessage `json:"id"`
		Kind          json.RawMessage `json:"kind"`
		SchemaVersion json.RawMessage `json:"schemaVersion"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return nil
	}
	var cfg CustomRuleConfig
	_ = json.Unmarshal(head.ID, &cfg.ID)
	_ = json.Unmarshal(head.Kind, &cfg.Kind)
	_ = json.Unmarshal(head.SchemaVersion, &cfg.SchemaVersion)
	p := checkCustomRuleVersion(cfg)
	if p == nil || cfg.SchemaVersion < 0 {
		return nil
	}
	return p
}

// IsNewerRelease reports whether e is a decode error of a newer release's rule (CustomRuleEntry.Err): a kind or
// schemaVersion this build cannot run. Only meaningful on decode errors, where UNSUPPORTED_VERSION is never a
// negative schemaVersion.
func (e *CustomRuleError) IsNewerRelease() bool {
	return e != nil && (e.Reason == ReasonUnknownKind || e.Reason == ReasonUnsupportedVersion)
}

// builtinRuleIDs is the set of built-in and library rule IDs. A custom rule under one of them would share that
// rule's settings entry, suppressions, stage and finding identity.
var builtinRuleIDs = sync.OnceValue(func() map[string]bool {
	ids := make(map[string]bool)
	for _, r := range AllRules() {
		ids[r.ID()] = true
	}
	for _, r := range LibraryRules() {
		ids[r.ID()] = true
	}
	return ids
})

// checkCustomRuleVersion refuses, in every mode, a kind or schemaVersion this build cannot run: running it as a
// block rule would match blocks it was never meant to.
func checkCustomRuleVersion(cfg CustomRuleConfig) *CustomRuleError {
	n := cfg.Normalized()
	newest, ok := customRuleKinds[n.Kind]
	if !ok {
		return &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: "kind", Reason: ReasonUnknownKind,
			Msg: fmt.Sprintf("custom rule %q: unknown kind %q (this version reads %q)", cfg.ID, cfg.Kind, CustomRuleKindBlock)}
	}
	if n.SchemaVersion < 1 {
		return &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: "schemaVersion", Reason: ReasonUnsupportedVersion,
			Msg: fmt.Sprintf("custom rule %q: schemaVersion %d is not valid", cfg.ID, cfg.SchemaVersion)}
	}
	if n.SchemaVersion > newest {
		return &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: "schemaVersion", Reason: ReasonUnsupportedVersion,
			Msg: fmt.Sprintf("custom rule %q: schemaVersion %d of kind %q is newer than this version reads (%d)",
				cfg.ID, cfg.SchemaVersion, n.Kind, newest)}
	}
	return nil
}

// checkCustomRuleShape reports the problems the mode decides between error and warning: unknown fields, a missing
// id, a built-in rule's id and (Strict only, always a warning) a rule without a matcher.
func checkCustomRuleShape(cfg CustomRuleConfig, mode CustomRuleMode) []*CustomRuleError {
	tolerant := mode == CustomRuleTolerant
	var out []*CustomRuleError
	for _, key := range cfg.UnknownFields() {
		msg := fmt.Sprintf("custom rule %q: unknown field %q", cfg.ID, key)
		if tolerant {
			msg += " ignored"
		}
		out = append(out, &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: key, Reason: ReasonUnknownField,
			Msg: msg, Warning: tolerant})
	}
	switch {
	case strings.TrimSpace(cfg.ID) == "":
		msg := "custom rule has no id; the id is the rule id on its findings"
		if tolerant {
			msg = "custom rule has no id; its findings carry an empty rule id"
		}
		out = append(out, &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: "id", Reason: ReasonMissingID,
			Msg: msg, Warning: tolerant})
	case builtinRuleIDs()[cfg.ID]:
		msg := fmt.Sprintf("custom rule %q: the id is a built-in rule's id", cfg.ID)
		if tolerant {
			msg += "; both run under it"
		}
		out = append(out, &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: "id", Reason: ReasonReservedID,
			Msg: msg, Warning: tolerant})
	}
	if !tolerant && cfg.TypeMatch == "" && cfg.RawTypeMatch == "" && cfg.NameMatch == "" &&
		len(cfg.PropertyHas) == 0 && len(cfg.PropertyNot) == 0 {
		out = append(out, &CustomRuleError{Index: -1, RuleID: cfg.ID, Reason: ReasonNoMatcher, Warning: true,
			Msg: fmt.Sprintf("custom rule %q: no typeMatch, rawTypeMatch, nameMatch, propertyHas or propertyNot; it matches every block", cfg.ID)})
	}
	return out
}
