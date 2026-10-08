// Package settingsdoc stores the settings document (AppSettings) as sections, one per top-level key, each with
// its own version (ADR 0012). The sections are declared in sections.go. A store reads a section stored at an older
// version through the section's upgrades and one stored at a newer version as it is, and merges a write onto what
// it holds, so the fields and the version a newer binary stored survive a write by an older one. The package is a
// leaf: the Postgres backend and the desktop file store both use it.
package settingsdoc

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

// LegacyVersion is the version of every section of a document stored whole, before sections had versions: the
// legacy blob rows (app_settings, user_settings, org_settings) and a desktop file without section versions.
const LegacyVersion = 1

// DocVersionKey is the document's own top-level version (AppSettings.Version). It is not a section and stays 1;
// the sections carry the versions that change.
const DocVersionKey = "version"

// Upgrade turns a section's data stored at the version below the one it is registered under into that version.
// The data may be JSON null (a section stored at its zero value).
type Upgrade func(data json.RawMessage) (json.RawMessage, error)

// Section declares one top-level key of the settings document.
type Section struct {
	Key string
	// Version is the version this binary reads and writes: LegacyVersion until the section's stored shape changes.
	Version int
	// Upgrades[v] turns data stored at version v-1 into version v, for every v above LegacyVersion up to Version.
	Upgrades map[int]Upgrade
}

// Stored is a section as a store holds it.
type Stored struct {
	Version int
	Data    json.RawMessage
}

// Registry is the set of sections a binary knows.
type Registry struct {
	byKey map[string]Section
}

// NewRegistry checks the declarations: unique keys, a version of at least LegacyVersion, and exactly one upgrade
// per version step.
func NewRegistry(sections ...Section) (*Registry, error) {
	r := &Registry{byKey: make(map[string]Section, len(sections))}
	for _, s := range sections {
		if s.Key == "" || s.Key == DocVersionKey {
			return nil, fmt.Errorf("settingsdoc: invalid section key %q", s.Key)
		}
		if _, dup := r.byKey[s.Key]; dup {
			return nil, fmt.Errorf("settingsdoc: section %q declared twice", s.Key)
		}
		if s.Version < LegacyVersion {
			return nil, fmt.Errorf("settingsdoc: section %q has version %d, below %d", s.Key, s.Version, LegacyVersion)
		}
		for v := LegacyVersion + 1; v <= s.Version; v++ {
			if s.Upgrades[v] == nil {
				return nil, fmt.Errorf("settingsdoc: section %q has no upgrade to version %d", s.Key, v)
			}
		}
		for v := range s.Upgrades {
			if v <= LegacyVersion || v > s.Version {
				return nil, fmt.Errorf("settingsdoc: section %q has an upgrade to version %d outside %d..%d", s.Key, v, LegacyVersion+1, s.Version)
			}
		}
		r.byKey[s.Key] = s
	}
	return r, nil
}

// MustRegistry is NewRegistry for declarations fixed at compile time.
func MustRegistry(sections ...Section) *Registry {
	r, err := NewRegistry(sections...)
	if err != nil {
		panic(err)
	}
	return r
}

// Section returns the declaration of key. A key nobody declared reads and writes at LegacyVersion with no
// upgrades; TestSections_CoverTheDocument keeps every key of AppSettings declared.
func (r *Registry) Section(key string) Section {
	if s, ok := r.byKey[key]; ok {
		return s
	}
	return Section{Key: key, Version: LegacyVersion}
}

// Keys returns the declared section keys in order.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.byKey))
	for k := range r.byKey {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Upgrade brings s to the version this binary knows through the section's upgrades. A section at the current
// version or a newer one (a newer binary wrote it) is returned unchanged.
func (r *Registry) Upgrade(key string, s Stored) (Stored, error) {
	sec := r.Section(key)
	if s.Version < LegacyVersion {
		s.Version = LegacyVersion
	}
	if len(s.Data) == 0 {
		s.Data = json.RawMessage("null")
	}
	for v := s.Version + 1; v <= sec.Version; v++ {
		data, err := sec.Upgrades[v](s.Data)
		if err != nil {
			return Stored{}, fmt.Errorf("settings: upgrade section %q to version %d: %w", key, v, err)
		}
		s = Stored{Version: v, Data: data}
	}
	return s, nil
}

// Decode sets the fields of *doc, a settings document struct, from sections, each upgraded first. A field whose
// section is absent keeps the value doc holds; a present one is replaced, not merged into. A section stored at a
// newer version decodes the fields this binary knows, and a field whose JSON type a newer version changed keeps
// its zero value rather than failing the read. Sections doc has no field for are skipped. Decode returns the
// sections as upgraded, the base for a later Encode.
func (r *Registry) Decode(sections map[string]Stored, doc any) (map[string]Stored, error) {
	rv := reflect.ValueOf(doc)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("settingsdoc: Decode needs a non-nil pointer to a struct, got %T", doc)
	}
	fields := fieldsOf(rv.Elem().Type())
	out := make(map[string]Stored, len(sections))
	for key, s := range sections {
		up, err := r.Upgrade(key, s)
		if err != nil {
			return nil, err
		}
		out[key] = up
		f, ok := fields.byName[key]
		if !ok {
			continue
		}
		v := reflect.New(f.typ)
		if err := json.Unmarshal(up.Data, v.Interface()); err != nil {
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) || up.Version <= r.Section(key).Version {
				return nil, fmt.Errorf("settings: section %q: %w", key, err)
			}
		}
		rv.Elem().FieldByIndex(f.index).Set(v.Elem())
	}
	return out, nil
}

// Encode returns doc's sections to store, each merged onto its counterpart in stored (upgraded first when older):
// the fields this binary does not know are kept from stored, and so are a newer stored version and a field whose
// stored value that version gave a type this binary cannot decode. Sections in stored that doc has no key for are
// not returned; a store that rewrites everything it holds keeps them itself.
func (r *Registry) Encode(doc any, stored map[string]Stored) (map[string]Stored, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("settingsdoc: marshal: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("settingsdoc: %T is not a JSON object: %w", doc, err)
	}
	fields := fieldsOf(indirect(reflect.TypeOf(doc)))
	out := make(map[string]Stored, len(top))
	for key, data := range top {
		if key == DocVersionKey {
			continue
		}
		sec := r.Section(key)
		prev, ok := stored[key]
		if !ok {
			out[key] = Stored{Version: sec.Version, Data: data}
			continue
		}
		if prev, err = r.Upgrade(key, prev); err != nil {
			return nil, err
		}
		out[key] = Stored{
			Version: max(prev.Version, sec.Version),
			Data:    merge(fields.byName[key].typ, prev.Data, data, prev.Version > sec.Version),
		}
	}
	return out, nil
}

// DocumentKeys returns the top-level JSON keys of doc's type in the order encoding/json writes them.
func DocumentKeys(doc any) []string {
	fields := fieldsOf(indirect(reflect.TypeOf(doc)))
	keys := make([]string, len(fields.list))
	for i, f := range fields.list {
		keys[i] = f.name
	}
	return keys
}

// merge returns written with what stored holds beyond t carried over. For a struct, the fields t declares come from
// written (absent there means absent: an omitempty zero) and every other key from stored; with keepUndecodable, a
// declared field whose stored value does not decode into its type is kept from stored. A map with struct or map
// values keeps written's keys and merges each value with stored's. Anything else, and anything that is not the JSON
// shape t expects, is written as given.
func merge(t reflect.Type, stored, written json.RawMessage, keepUndecodable bool) json.RawMessage {
	if t == nil || isNull(stored) || isNull(written) || opaque(t) {
		return written
	}
	t = indirect(t)
	switch t.Kind() {
	case reflect.Struct:
		var so, wo map[string]json.RawMessage
		if json.Unmarshal(stored, &so) != nil || json.Unmarshal(written, &wo) != nil || so == nil || wo == nil {
			return written
		}
		fields := fieldsOf(t)
		var obj objectWriter
		for _, f := range fields.list {
			w, ok := wo[f.name]
			if !ok {
				continue
			}
			if s, ok := so[f.name]; ok {
				if keepUndecodable && !decodes(s, f.typ) {
					w = s
				} else {
					w = merge(f.typ, s, w, keepUndecodable)
				}
			}
			obj.add(f.name, w)
		}
		extra := map[string]json.RawMessage{}
		for k, v := range so {
			if !fields.knows(k) {
				extra[k] = v
			}
		}
		for k, v := range wo {
			if _, ok := fields.byName[k]; !ok {
				extra[k] = v
			}
		}
		for _, k := range sortedKeys(extra) {
			obj.add(k, extra[k])
		}
		return obj.bytes()
	case reflect.Map:
		elem := indirect(t.Elem())
		if t.Key().Kind() != reflect.String || opaque(t.Elem()) || (elem.Kind() != reflect.Struct && elem.Kind() != reflect.Map) {
			return written
		}
		var so, wo map[string]json.RawMessage
		if json.Unmarshal(stored, &so) != nil || json.Unmarshal(written, &wo) != nil || wo == nil {
			return written
		}
		var obj objectWriter
		for _, k := range sortedKeys(wo) {
			w := wo[k]
			if s, ok := so[k]; ok {
				w = merge(t.Elem(), s, w, keepUndecodable)
			}
			obj.add(k, w)
		}
		return obj.bytes()
	default:
		return written
	}
}

// objectWriter writes a JSON object key by key in the order given, as encoding/json lays out a struct.
type objectWriter struct {
	buf bytes.Buffer
	n   int
}

func (o *objectWriter) add(key string, value json.RawMessage) {
	if o.n == 0 {
		o.buf.WriteByte('{')
	} else {
		o.buf.WriteByte(',')
	}
	k, _ := json.Marshal(key)
	o.buf.Write(k)
	o.buf.WriteByte(':')
	o.buf.Write(value)
	o.n++
}

func (o *objectWriter) bytes() json.RawMessage {
	if o.n == 0 {
		return json.RawMessage("{}")
	}
	o.buf.WriteByte('}')
	return o.buf.Bytes()
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func decodes(raw json.RawMessage, t reflect.Type) bool {
	return json.Unmarshal(raw, reflect.New(t).Interface()) == nil
}

func indirect(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

var (
	jsonMarshaler   = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
)

// opaque reports a type that encodes itself (time.Time, json.RawMessage): its JSON is not its fields.
func opaque(t reflect.Type) bool {
	return t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler)
}

// field is one JSON key of a struct as encoding/json sees it.
type field struct {
	name  string
	index []int
	typ   reflect.Type
}

type structFields struct {
	list   []field
	byName map[string]field
	// folded holds the lower-cased names: encoding/json also decodes a key into a field whose name differs only in
	// case, so such a stored key is not a field this binary lacks.
	folded map[string]bool
}

func (s *structFields) knows(key string) bool {
	return s.folded[strings.ToLower(key)]
}

var fieldCache sync.Map // reflect.Type -> *structFields

// fieldsOf lists the JSON keys of struct type t in encoding order: exported fields, tag names, "-" skipped, and the
// fields of an untagged embedded struct promoted.
func fieldsOf(t reflect.Type) *structFields {
	if cached, ok := fieldCache.Load(t); ok {
		return cached.(*structFields)
	}
	sf := &structFields{byName: map[string]field{}, folded: map[string]bool{}}
	if t != nil && t.Kind() == reflect.Struct {
		collectFields(t, nil, sf)
	}
	cached, _ := fieldCache.LoadOrStore(t, sf)
	return cached.(*structFields)
}

func collectFields(t reflect.Type, prefix []int, sf *structFields) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		index := append(slices.Clone(prefix), i)
		if f.Anonymous && name == "" && indirect(f.Type).Kind() == reflect.Struct && f.Type.Kind() != reflect.Pointer {
			collectFields(f.Type, index, sf)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := sf.byName[name]; dup {
			continue
		}
		fl := field{name: name, index: index, typ: f.Type}
		sf.list = append(sf.list, fl)
		sf.byName[name] = fl
		sf.folded[strings.ToLower(name)] = true
	}
}
