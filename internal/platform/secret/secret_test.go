package secret

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

const sentinel = "s3ntinel-hunter2"

// holder is the shape secrets travel in: config sections (exported fields), service structs (unexported fields),
// lists and maps.
type holder struct {
	Exported   Value
	unexported Value
	List       []Value
	hidden     []Value
	Ptr        *Value
	Map        map[string]Value
}

func newHolder() holder {
	v := New(sentinel)
	return holder{
		Exported: v, unexported: v,
		List: []Value{v}, hidden: []Value{v},
		Ptr: &v, Map: map[string]Value{"k": v},
	}
}

// holderError is an error that prints the struct it carries, as a careless error type would.
type holderError struct{ h holder }

func (e holderError) Error() string { return fmt.Sprintf("bad config %+v", e.h) }

func TestValue_FormatNeverShowsTheSecret(t *testing.T) {
	t.Parallel()
	v := New(sentinel)
	h := newHolder()
	operands := map[string]any{
		"value": v, "pointer": &v, "struct": h, "struct pointer": &h,
		"slice": []Value{v}, "map": map[string]Value{"k": v}, "any slice": []any{v, &v},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v", "%10s", "%-12v", "%.4s", "%+q", "%#q"}
	for name, op := range operands {
		for _, verb := range verbs {
			if got := fmt.Sprintf(verb, op); strings.Contains(got, sentinel) || strings.Contains(got, "hunter") {
				t.Errorf("%s with %s shows the secret: %s", name, verb, got)
			}
		}
		for _, got := range []string{fmt.Sprint(op), fmt.Sprintln(op)} {
			if strings.Contains(got, sentinel) {
				t.Errorf("%s with Sprint/Sprintln shows the secret: %s", name, got)
			}
		}
	}
	for verb, want := range map[string]string{"%v": Redacted, "%s": Redacted, "%+v": Redacted, "%q": `"` + Redacted + `"`} {
		if got := fmt.Sprintf(verb, v); got != want {
			t.Errorf("Sprintf(%q) = %q, want %q", verb, got, want)
		}
	}
	if got := v.String(); got != Redacted {
		t.Errorf("String() = %q, want %q", got, Redacted)
	}
	if got := v.GoString(); strings.Contains(got, sentinel) || !strings.Contains(got, Redacted) {
		t.Errorf("GoString() = %q", got)
	}
}

// An unset value prints as empty, so a dump still tells set from unset without showing either.
func TestValue_UnsetStaysEmpty(t *testing.T) {
	t.Parallel()
	var zero Value
	for _, v := range []Value{zero, New("")} {
		if v.IsSet() {
			t.Error("IsSet() = true for an empty value")
		}
		if got := fmt.Sprintf("%v|%s|%q", v, v, v); got != `||""` {
			t.Errorf("unset formats as %q", got)
		}
		if raw, err := json.Marshal(v); err != nil || string(raw) != `""` {
			t.Errorf("unset marshals as %s, %v", raw, err)
		}
		if v.Reveal() != "" {
			t.Errorf("Reveal() = %q", v.Reveal())
		}
	}
	if !New(sentinel).IsSet() {
		t.Error("IsSet() = false for a set value")
	}
}

func TestValue_RevealIsTheOnlyWayOut(t *testing.T) {
	t.Parallel()
	if got := New(sentinel).Reveal(); got != sentinel {
		t.Errorf("Reveal() = %q", got)
	}
	// Copies share the secret: a Value is passed around by value like the string it replaces.
	v := New(sentinel)
	w := v
	if w.Reveal() != sentinel {
		t.Error("a copy lost the secret")
	}
	if got := RevealAll([]Value{New("a"), {}, New("b")}); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Errorf("RevealAll = %q", got)
	}
	if RevealAll(nil) != nil {
		t.Error("RevealAll(nil) is not nil")
	}
}

// == on a Value could only compare identity, not the secret; it does not compile, so callers use Equal (constant
// time). reflect.DeepEqual (config tests) still compares the secrets.
func TestValue_NotComparable(t *testing.T) {
	t.Parallel()
	if reflect.TypeFor[Value]().Comparable() {
		t.Error("Value is comparable with ==")
	}
	if !reflect.DeepEqual(New(sentinel), New(sentinel)) {
		t.Error("DeepEqual does not see two equal secrets as equal")
	}
	if reflect.DeepEqual(New(sentinel), New("other")) {
		t.Error("DeepEqual sees different secrets as equal")
	}
}

func TestValue_Equal(t *testing.T) {
	t.Parallel()
	v := New(sentinel)
	if !v.Equal(sentinel) {
		t.Error("Equal(the secret) = false")
	}
	for _, other := range []string{"", "s3ntinel", sentinel + "x", strings.ToUpper(sentinel)} {
		if v.Equal(other) {
			t.Errorf("Equal(%q) = true", other)
		}
	}
	// An unset secret matches nothing, not even "": an unconfigured token must never admit an empty one.
	var unset Value
	if unset.Equal("") || unset.Equal("x") {
		t.Error("an unset value compares equal")
	}
}

func TestValue_JSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(newHolder())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Errorf("json shows the secret: %s", raw)
	}
	if !strings.Contains(string(raw), `"Exported":"`+Redacted+`"`) {
		t.Errorf("json = %s, want the exported field as %q", raw, Redacted)
	}

	// Config files hold plain strings.
	var cfg struct {
		Key  Value
		Keys []Value
		Null Value
	}
	cfg.Null = New("kept")
	if err := json.Unmarshal([]byte(`{"Key":"`+sentinel+`","Keys":["a","b"],"Null":null}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Key.Reveal() != sentinel || len(cfg.Keys) != 2 || cfg.Keys[1].Reveal() != "b" {
		t.Errorf("unmarshal = %q %q", cfg.Key.Reveal(), RevealAll(cfg.Keys))
	}
	if cfg.Null.Reveal() != "kept" {
		t.Errorf("null replaced the value: %q", cfg.Null.Reveal())
	}
	for _, bad := range []string{`{"Key":42}`, `{"Key":{"s":"x"}}`, `{"Key":["x"]}`} {
		if err := json.Unmarshal([]byte(bad), &cfg); err == nil {
			t.Errorf("unmarshal %s: no error", bad)
		}
	}
}

// A redacted copy written back as configuration must fail loudly, not become the secret "[redacted]".
func TestValue_RefusesTheRedactionMarker(t *testing.T) {
	t.Parallel()
	var v Value
	if err := json.Unmarshal([]byte(`"`+Redacted+`"`), &v); err == nil {
		t.Error("UnmarshalJSON accepted the redaction marker")
	}
	if err := v.UnmarshalText([]byte(Redacted)); err == nil {
		t.Error("UnmarshalText accepted the redaction marker")
	}
	if v.IsSet() {
		t.Error("a refused value was stored")
	}
}

func TestValue_Text(t *testing.T) {
	t.Parallel()
	text, err := New(sentinel).MarshalText()
	if err != nil || string(text) != Redacted {
		t.Errorf("MarshalText = %q, %v", text, err)
	}
	var v Value
	if err := v.UnmarshalText([]byte(sentinel)); err != nil || v.Reveal() != sentinel {
		t.Errorf("UnmarshalText: %q, %v", v.Reveal(), err)
	}
	// encoding/xml goes through TextMarshaler.
	type doc struct{ Key Value }
	raw, err := xml.Marshal(doc{New(sentinel)})
	if err != nil || strings.Contains(string(raw), sentinel) {
		t.Errorf("xml = %s, %v", raw, err)
	}
}

func TestValue_Slog(t *testing.T) {
	t.Parallel()
	v := New(sentinel)
	h := newHolder()
	for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(newHandler(&buf))
		log.Info("boot", "key", v, "ptr", &v, "cfg", h, "cfgptr", &h, slog.Group("g", "key", v))
		log.With("key", v).Info("with")
		log.Info("err", "error", fmt.Errorf("wrapped: %w", holderError{h}))
		if strings.Contains(buf.String(), sentinel) {
			t.Errorf("%s handler shows the secret:\n%s", name, buf.String())
		}
		if !strings.Contains(buf.String(), Redacted) {
			t.Errorf("%s handler never wrote the marker:\n%s", name, buf.String())
		}
	}
	if got := v.LogValue(); got.Kind() != slog.KindString || got.String() != Redacted {
		t.Errorf("LogValue() = %v", got)
	}
}

func TestValue_Errors(t *testing.T) {
	t.Parallel()
	v := New(sentinel)
	h := newHolder()
	inner := holderError{h}
	for _, err := range []error{
		fmt.Errorf("auth: key %v rejected", v),
		fmt.Errorf("auth: key %q rejected", v),
		fmt.Errorf("load: %w", inner),
		errors.Join(errors.New("first"), inner, fmt.Errorf("cfg %#v", h)),
	} {
		if strings.Contains(err.Error(), sentinel) || strings.Contains(fmt.Sprintf("%+v", err), sentinel) {
			t.Errorf("error shows the secret: %v", err)
		}
	}
}
