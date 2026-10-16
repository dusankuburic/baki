package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Duration is a configuration duration: parsed once, when the configuration loads, so its readers get a
// time.Duration (Std) and never parse, default or misread it on their own. Config files and PAD_* variables write it
// as a Go duration string ("30s", "5m", "1h"); a config file may also give an integer count of nanoseconds, the
// encoding of a time.Duration. An empty string leaves the setting unset: the default stays.
type Duration time.Duration

// Std is the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String is the duration in Go's notation ("1m30s").
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes the string form, what a config file holds.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

var durationType = reflect.TypeFor[Duration]()

// UnmarshalJSON reads a duration string, or an integer count of nanoseconds. "" and null leave the default, as an
// empty PAD_* variable does: a config file written from the struct before Duration holds "" for every duration it
// did not set. A bad value is a *json.UnmarshalTypeError, so the decoder's error names the field.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	switch {
	case s == "null":
		return nil
	case len(s) > 0 && s[0] == '"':
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if str == "" {
			return nil
		}
		v, err := time.ParseDuration(str)
		if err != nil {
			return &json.UnmarshalTypeError{Value: fmt.Sprintf("string %q (a duration is written like \"30s\", \"5m\" or \"1h\")", str), Type: durationType}
		}
		*d = Duration(v)
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return &json.UnmarshalTypeError{Value: jsonKind(s), Type: durationType}
	}
	*d = Duration(n)
	return nil
}

// UnmarshalText reads a duration string, the form of a PAD_* variable; empty leaves the default.
func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// jsonKind describes a JSON value that is not a duration, as encoding/json's own type errors do.
func jsonKind(s string) string {
	if s == "" {
		return "nothing"
	}
	switch s[0] {
	case 't', 'f':
		return "bool"
	case '[':
		return "array"
	case '{':
		return "object"
	}
	return "number " + s
}

// OptionalDuration is a Duration that tells "not set" from an explicit "0s", for a setting whose default is derived
// from other settings once the configuration has loaded (Storage.DBIdleTxTimeout follows the request timeout), so
// no default can be written into Default before the sources are read. "" and null leave it unset, as they leave a
// Duration's default; it marshals as null when unset, so a configuration written out and read back keeps the
// difference.
type OptionalDuration struct {
	d   Duration
	set bool
}

// ExplicitDuration is d as an explicitly set OptionalDuration.
func ExplicitDuration(d time.Duration) OptionalDuration {
	return OptionalDuration{d: Duration(d), set: true}
}

// Get is the duration and whether it was set.
func (o OptionalDuration) Get() (time.Duration, bool) { return o.d.Std(), o.set }

// IsSet reports whether the setting was given.
func (o OptionalDuration) IsSet() bool { return o.set }

// String is the duration in Go's notation, or "unset".
func (o OptionalDuration) String() string {
	if !o.set {
		return "unset"
	}
	return o.d.String()
}

// MarshalJSON writes null when unset, else the string form.
func (o OptionalDuration) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	return o.d.MarshalJSON()
}

// UnmarshalJSON reads what Duration reads; "" and null leave it unset.
func (o *OptionalDuration) UnmarshalJSON(b []byte) error {
	if s := string(b); s == "null" || s == `""` {
		return nil
	}
	var d Duration
	if err := d.UnmarshalJSON(b); err != nil {
		return err
	}
	*o = OptionalDuration{d: d, set: true}
	return nil
}

// UnmarshalText reads a duration string, the form of a PAD_* variable; empty leaves it unset.
func (o *OptionalDuration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var d Duration
	if err := d.UnmarshalText(b); err != nil {
		return err
	}
	*o = OptionalDuration{d: d, set: true}
	return nil
}
