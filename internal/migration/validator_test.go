package migration_test

import (
	"encoding/json"
	"testing"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
)

func TestValidator_ValidFlow(t *testing.T) {
	v := migration.NewValidator()
	flow := &interfaces.FlowDocument{
		ID:      "f1",
		Name:    "My Flow",
		Content: []byte(`{"subflows":[]}`),
	}
	if errs := v.ValidateFlow(flow); len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidator_EmptyID(t *testing.T) {
	v := migration.NewValidator()
	errs := v.ValidateFlow(&interfaces.FlowDocument{Name: "X", Content: []byte("{}")})
	if len(errs) == 0 {
		t.Error("expected error for empty ID")
	}
}

func TestValidator_EmptyName(t *testing.T) {
	v := migration.NewValidator()
	errs := v.ValidateFlow(&interfaces.FlowDocument{ID: "f1", Content: []byte("{}")})
	if len(errs) == 0 {
		t.Error("expected error for empty name")
	}
}

func TestValidator_InvalidJSON(t *testing.T) {
	v := migration.NewValidator()
	errs := v.ValidateFlow(&interfaces.FlowDocument{ID: "f1", Name: "F", Content: []byte("not json")})
	if len(errs) == 0 {
		t.Error("expected error for invalid JSON content")
	}
}

func TestValidator_RepairEmptyName(t *testing.T) {
	v := migration.NewValidator()
	flow := &interfaces.FlowDocument{ID: "f1", Content: []byte("{}")}
	repaired, fixes := v.RepairFlow(flow)
	if repaired.Name == "" {
		t.Error("RepairFlow should assign a default name")
	}
	if len(fixes) == 0 {
		t.Error("expected at least one fix")
	}
}

func TestValidator_RepairTrailingComma(t *testing.T) {
	v := migration.NewValidator()
	flow := &interfaces.FlowDocument{
		ID:      "f1",
		Name:    "F",
		Content: []byte(`{"key":"value"},`),
	}
	repaired, _ := v.RepairFlow(flow)
	if !json.Valid(repaired.Content) {
		t.Errorf("expected valid JSON after repair, got: %s", repaired.Content)
	}
}
