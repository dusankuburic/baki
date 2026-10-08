package service

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"pad-core/models"
)

// TestSettingsRoundTrip guards against the storage-layer and domain settings structs
// drifting apart: settingsToModel/settingsFromModel bridge them via a JSON round-trip, so
// a field present on one struct but missing (or mis-tagged) on the other would be
// silently dropped; converting fully-populated DefaultSettings down and back must
// reproduce it exactly. JSON serializations are compared rather than reflect.DeepEqual —
// RuleConfig.Options is map[string]interface{} and a round-trip legitimately normalizes
// its numerics (int 6 -> float64 6); marshaled output ignores that while still catching
// any actually-dropped field.
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

// TestSettingsRoundTrip_EveryFieldSurvives fills every field of the domain settings with a non-zero value before
// the round trip: DefaultSettings leaves omitempty fields at zero, so a field missing from the storage struct
// (perUserDailyBudget was, and a cloud save dropped the per-user cap) passed the test above.
func TestSettingsRoundTrip_EveryFieldSurvives(t *testing.T) {
	var original models.AppSettings
	fillNonZero(reflect.ValueOf(&original).Elem())
	want, _ := json.Marshal(&original)
	got, _ := json.Marshal(settingsToModel(settingsFromModel(&original)))
	if string(want) != string(got) {
		t.Errorf("a field was lost between the domain and storage settings structs:\n want=%s\n got =%s", want, got)
	}
}

// fillNonZero sets every exported field reachable from v to a non-zero value, maps and slices to one entry.
func fillNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillNonZero(v.Field(i))
			}
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		elem := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(elem)
		m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), elem)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf("v"))
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
