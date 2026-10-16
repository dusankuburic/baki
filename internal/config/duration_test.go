package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDuration_UnmarshalJSON(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"1h30m"`, 90 * time.Minute},
		{`"0s"`, 0},
		{`"-5s"`, -5 * time.Second}, // parsed; Validate judges the range
		// An integer is a nanosecond count, what encoding/json writes for a time.Duration.
		{`1500000000`, 1500 * time.Millisecond},
		{`0`, 0},
	} {
		d := Duration(time.Hour)
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if d.Std() != tc.want {
			t.Errorf("%s = %v, want %v", tc.in, d.Std(), tc.want)
		}
	}
}

// TestDuration_EmptyLeavesTheDefault: an empty string (or null) is a setting left unset, as an empty PAD_* variable
// is: the default stays. A config file written from the old struct holds "" for every duration it did not set.
func TestDuration_EmptyLeavesTheDefault(t *testing.T) {
	for _, in := range []string{`""`, `null`} {
		d := Duration(42 * time.Second)
		if err := json.Unmarshal([]byte(in), &d); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if d.Std() != 42*time.Second {
			t.Errorf("%s replaced the default: %v", in, d)
		}
	}
	d := Duration(42 * time.Second)
	if err := d.UnmarshalText(nil); err != nil || d.Std() != 42*time.Second {
		t.Errorf("empty text: %v, %v; want the default kept", d, err)
	}
}

// TestDuration_InvalidNamesTheField: a bad duration in a config file is refused, and the error names the field.
func TestDuration_InvalidNamesTheField(t *testing.T) {
	for _, in := range []string{`"every hour"`, `"30"`, `1.5`, `true`, `[]`} {
		var v struct {
			Governance struct{ ScanInterval Duration }
		}
		err := json.Unmarshal([]byte(`{"Governance":{"ScanInterval":`+in+`}}`), &v)
		if err == nil {
			t.Errorf("%s accepted as %v", in, v.Governance.ScanInterval)
			continue
		}
		if !strings.Contains(err.Error(), "Governance.ScanInterval") {
			t.Errorf("%s: %v; want it to name Governance.ScanInterval", in, err)
		}
	}
}

func TestDuration_UnmarshalText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("250ms")); err != nil || d.Std() != 250*time.Millisecond {
		t.Errorf("250ms = %v, %v", d, err)
	}
	if err := d.UnmarshalText([]byte("1500000000")); err == nil {
		t.Error("an environment value is a duration string; a bare number was accepted")
	}
}

func TestDuration_MarshalJSONAndString(t *testing.T) {
	b, err := json.Marshal(struct{ D Duration }{Duration(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"D":"1m30s"}` {
		t.Errorf("marshal = %s, want the string form", b)
	}
	var back struct{ D Duration }
	if err := json.Unmarshal(b, &back); err != nil || back.D.Std() != 90*time.Second {
		t.Errorf("round trip = %v, %v", back.D, err)
	}
	if got := Duration(0).String(); got != "0s" {
		t.Errorf("String() = %q", got)
	}
}

// TestOptionalDuration_UnsetDiffersFromZero: an OptionalDuration keeps "not set" apart from an explicit "0s", which
// a Duration cannot. "" and null leave it unset, as they leave a Duration's default; it marshals back as null when
// unset and as the string form when set, so a configuration written out and read back keeps the difference; a bad
// value names the field.
func TestOptionalDuration_UnsetDiffersFromZero(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  time.Duration
		isSet bool
	}{
		{`{}`, 0, false},
		{`{"D":null}`, 0, false},
		{`{"D":""}`, 0, false},
		{`{"D":"0s"}`, 0, true},
		{`{"D":0}`, 0, true},
		{`{"D":"90s"}`, 90 * time.Second, true},
	} {
		var v struct{ D OptionalDuration }
		if err := json.Unmarshal([]byte(tc.in), &v); err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got, set := v.D.Get(); got != tc.want || set != tc.isSet {
			t.Errorf("%s = (%v, %v), want (%v, %v)", tc.in, got, set, tc.want, tc.isSet)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var back struct{ D OptionalDuration }
		if err := json.Unmarshal(b, &back); err != nil || back.D != v.D {
			t.Errorf("%s: round trip through %s = %+v, %v", tc.in, b, back.D, err)
		}
	}
	if b, _ := json.Marshal(struct{ D OptionalDuration }{}); string(b) != `{"D":null}` {
		t.Errorf("unset marshals as %s, want null", b)
	}
	if b, _ := json.Marshal(struct{ D OptionalDuration }{ExplicitDuration(0)}); string(b) != `{"D":"0s"}` {
		t.Errorf(`an explicit 0 marshals as %s, want "0s"`, b)
	}
	var v struct {
		Storage struct{ DBIdleTxTimeout OptionalDuration }
	}
	if err := json.Unmarshal([]byte(`{"Storage":{"DBIdleTxTimeout":"soon"}}`), &v); err == nil || !strings.Contains(err.Error(), "Storage.DBIdleTxTimeout") {
		t.Errorf("a bad value: %v; want an error naming Storage.DBIdleTxTimeout", err)
	}
	var o OptionalDuration
	if err := o.UnmarshalText(nil); err != nil || o.IsSet() {
		t.Errorf("empty text: %+v, %v; want unset", o, err)
	}
	if err := o.UnmarshalText([]byte("2m")); err != nil {
		t.Fatal(err)
	}
	if got, set := o.Get(); got != 2*time.Minute || !set {
		t.Errorf("2m = (%v, %v)", got, set)
	}
}
