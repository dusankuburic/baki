// Package secret holds credentials (keys, passwords, tokens, DSNs and webhook URLs that carry one) so that they
// cannot be printed by accident: a Value formats, marshals and logs as "[redacted]" in every form, and Reveal is the
// one way to read it, called at the boundary that needs the plain text (a driver, a signer, a keystore).
//
// An unset Value prints as empty, so a dump of a configuration still tells set from unset. Config files and
// environment variables hold plain strings: UnmarshalJSON and UnmarshalText accept them.
package secret

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// Redacted is what a set Value shows instead of its secret.
const Redacted = "[redacted]"

// Value is a secret string. The zero Value is unset.
//
// The secret sits behind a pointer: fmt does not call methods on a value it reaches through an unexported struct
// field, so a struct holding a Value in an unexported field and printed with %+v shows the field's own
// representation, and a pointer prints as an address where a string would print as itself. The zero-length func
// array makes == a compile error: on the pointer it would compare identity, not the secret (use Equal).
type Value struct {
	_ [0]func()
	p *string
}

// New returns a Value holding s; New("") is unset.
func New(s string) Value {
	if s == "" {
		return Value{}
	}
	return Value{p: &s}
}

// Reveal returns the secret in plain text, "" when unset. Call it where the secret leaves the process or reaches a
// library that needs it, never to log or format it.
func (v Value) Reveal() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

// RevealAll reveals a list of secrets, for the consumers that take plain strings.
func RevealAll(vs []Value) []string {
	if vs == nil {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Reveal()
	}
	return out
}

// IsSet reports whether v holds a non-empty secret.
func (v Value) IsSet() bool { return v.p != nil }

// Equal reports, in constant time, whether v holds other. An unset Value equals nothing, not even "": an
// unconfigured token must never admit a request that sends an empty one. Comparing digests keeps the time
// independent of the length too.
func (v Value) Equal(other string) bool {
	if v.p == nil {
		return false
	}
	a, b := sha256.Sum256([]byte(*v.p)), sha256.Sum256([]byte(other))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// shown is what every encoding of v writes: the marker when set, "" when unset.
func (v Value) shown() string {
	if v.p == nil {
		return ""
	}
	return Redacted
}

// String implements fmt.Stringer.
func (v Value) String() string { return v.shown() }

// GoString implements fmt.GoStringer for the printers that call it directly.
func (v Value) GoString() string { return fmt.Sprintf("secret.Value(%q)", v.shown()) }

// Format implements fmt.Formatter, so every verb and flag (%v %+v %#v %s %q %x …) prints the marker; width and
// flags apply to the marker as to a string.
func (v Value) Format(f fmt.State, verb rune) {
	if verb != 'q' {
		verb = 's'
	}
	fmt.Fprintf(f, fmt.FormatString(f, verb), v.shown())
}

// LogValue implements slog.LogValuer.
func (v Value) LogValue() slog.Value { return slog.StringValue(v.shown()) }

// MarshalJSON writes the marker (or "" when unset), never the secret.
func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.shown()) }

// MarshalText implements encoding.TextMarshaler like MarshalJSON.
func (v Value) MarshalText() ([]byte, error) { return []byte(v.shown()), nil }

// errMarker refuses a redacted copy written back as configuration: stored, the marker would become the secret.
var errMarker = errors.New("secret: the value is the redaction marker " + Redacted + ", not a secret (a redacted copy was saved in its place)")

// UnmarshalJSON reads a plain JSON string (a config file). null leaves v unchanged.
func (v *Value) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("secret: want a JSON string: %w", err)
	}
	return v.set(s)
}

// UnmarshalText implements encoding.TextUnmarshaler: the value of an environment variable, verbatim.
func (v *Value) UnmarshalText(text []byte) error { return v.set(string(text)) }

func (v *Value) set(s string) error {
	if s == Redacted {
		return errMarker
	}
	*v = New(s)
	return nil
}
