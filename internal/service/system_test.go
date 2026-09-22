package service

import (
	"encoding/json"
	"testing"

	"pad-core/models"
)

// TestSettingsRoundTrip guards against the storage-layer and domain settings
// structs drifting apart. settingsToModel/settingsFromModel bridge them via a
// JSON round-trip,
// so any field present on one struct but missing (or mis-tagged) on the other
// would be silently dropped. Converting a fully-populated DefaultSettings down
// to the storage struct and back must reproduce it exactly.
//
// We compare JSON serializations rather than reflect.DeepEqual: RuleConfig.Options
// is map[string]interface{}, and a JSON round-trip legitimately normalizes its
// numeric values (int 6 -> float64 6). Comparing marshaled output ignores that
// representational detail while still catching any actually-dropped field.
func TestSettingsRoundTrip(t *testing.T) {
	original := models.DefaultSettings()

	stored := settingsFromModel(original)
	if stored == nil {
		t.Fatal("settingsFromModel returned nil for valid settings")
	}
	roundTripped := settingsToModel(stored)

	wantJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	gotJSON, err := json.Marshal(roundTripped)
	if err != nil {
		t.Fatalf("marshal round-tripped: %v", err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("settings lost data in storage round-trip — structs have drifted.\n want=%s\n got =%s", wantJSON, gotJSON)
	}
}

// TestToModel_NilReturnsDefaults verifies nil storage settings yield usable
// defaults rather than a nil pointer.
func TestToModel_NilReturnsDefaults(t *testing.T) {
	if got := settingsToModel(nil); got == nil {
		t.Error("settingsToModel(nil) returned nil, want default settings")
	}
}

// TestFromModel_NilReturnsNil documents the inverse: a nil model maps to nil.
func TestFromModel_NilReturnsNil(t *testing.T) {
	if got := settingsFromModel(nil); got != nil {
		t.Errorf("settingsFromModel(nil) = %+v, want nil", got)
	}
}
