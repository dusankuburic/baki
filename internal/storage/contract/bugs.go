package contract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// runBugScenarios pins storage bugs found in Phase 3 wave 1: a policy lost its waivers and severity budgets on
// Postgres, and the legacy migration stamped every copied flow with the time of the run (ImportFlow).
func runBugScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("ImportFlow_keeps_the_times_and_never_overwrites", func(t *testing.T) {
		owner := "contract-import-owner-" + runID
		id := "contract-import-" + runID
		created := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
		updated := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
		doc := &interfaces.FlowDocument{
			ID: id, Name: "Imported", Content: json.RawMessage(`{"subflows":[]}`), OwnerID: owner,
			CreatedAt: created, UpdatedAt: updated, Version: 7,
		}
		if err := b.ImportFlow(ctx, doc); err != nil {
			t.Fatalf("ImportFlow: %v", err)
		}
		if doc.Version != 0 || !doc.CreatedAt.Equal(created) || !doc.UpdatedAt.Equal(updated) {
			t.Errorf("after the import the document says version %d, %v / %v", doc.Version, doc.CreatedAt, doc.UpdatedAt)
		}
		got, err := b.LoadFlow(ctx, id)
		if err != nil {
			t.Fatalf("LoadFlow: %v", err)
		}
		if got.Version != 0 || got.Name != "Imported" || got.OwnerID != owner || !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
			t.Errorf("stored = version %d, %q by %q, %v / %v; want version 0 with the imported times", got.Version, got.Name, got.OwnerID, got.CreatedAt, got.UpdatedAt)
		}
		if listed, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, MetadataOnly: true}); err != nil || len(listed) != 1 || !listed[0].UpdatedAt.Equal(updated) {
			t.Errorf("ListFlows = %+v, %v; want the flow updated at %v", listed, err, updated)
		}

		// A stored flow is never overwritten, whatever version the import carries.
		for _, v := range []int{0, 7} {
			again := &interfaces.FlowDocument{ID: id, Name: "Other", Content: json.RawMessage(`{}`), OwnerID: owner, Version: v}
			if err := b.ImportFlow(ctx, again); !errors.Is(err, interfaces.ErrVersionConflict) {
				t.Errorf("importing a stored flow (version %d) = %v, want ErrVersionConflict", v, err)
			}
		}
		if kept, _ := b.LoadFlowHeader(ctx, id); kept == nil || kept.Name != "Imported" || kept.Version != 0 {
			t.Errorf("after the refused imports = %+v, want the first import", kept)
		}

		// A later save stamps the time of the save and keeps the creation time.
		before := time.Now().Add(-time.Minute)
		got.Name = "Edited"
		if err := b.SaveFlow(ctx, got); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		if saved, err := b.LoadFlowHeader(ctx, id); err != nil || saved.Version != 1 || !saved.CreatedAt.Equal(created) || saved.UpdatedAt.Before(before) {
			t.Errorf("after a save = %+v, %v; want version 1 created %v and updated now", saved, err, created)
		}

		// Without times the import stamps now, and the update time is the creation time.
		bare := &interfaces.FlowDocument{ID: id + "-bare", Name: "Bare", Content: json.RawMessage(`{}`), OwnerID: owner}
		if err := b.ImportFlow(ctx, bare); err != nil {
			t.Fatalf("ImportFlow without times: %v", err)
		}
		if stored, err := b.LoadFlowHeader(ctx, bare.ID); err != nil || stored.CreatedAt.Before(before) || !stored.UpdatedAt.Equal(stored.CreatedAt) {
			t.Errorf("imported without times = %+v, %v; want created now and updated at the same time", stored, err)
		}
	})

	run("Policies_keep_waivers_and_severity_budgets", func(t *testing.T) {
		org := "contract-polw-" + runID
		expires := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
		zero, three, five := 0, 3, 5
		p := &models.Policy{
			ID: "contract-polw-" + runID, OrgID: org, Name: "Budgeted", GateSeverity: models.SeverityWarning,
			Rules: []models.PolicyRule{{RuleID: "todo-comment", Enabled: true, MaxCount: &three}},
			Waivers: []models.PolicyWaiver{
				{FindingKey: "fp-1", Reason: "accepted risk", ExpiresAt: &expires},
				{FindingKey: "rule:block-2"},
			},
			MaxErrors: &zero, MaxWarnings: &five,
		}
		if err := b.SavePolicy(ctx, p); err != nil {
			t.Fatalf("SavePolicy: %v", err)
		}
		t.Cleanup(func() { _ = b.DeletePolicy(context.Background(), org, p.ID) })
		// The stored policy is a copy: the caller's values change after the save without changing it.
		five = 6

		check := func(label string, got *models.Policy) {
			t.Helper()
			if len(got.Waivers) != 2 {
				t.Fatalf("%s: waivers = %+v, want 2", label, got.Waivers)
			}
			w0, w1 := got.Waivers[0], got.Waivers[1]
			if w0.FindingKey != "fp-1" || w0.Reason != "accepted risk" || w0.ExpiresAt == nil || !w0.ExpiresAt.Equal(expires) {
				t.Errorf("%s: first waiver = %+v, want fp-1 with its reason, expiring %v", label, w0, expires)
			}
			if w1.FindingKey != "rule:block-2" || w1.Reason != "" || w1.ExpiresAt != nil {
				t.Errorf("%s: second waiver = %+v, want rule:block-2 alone", label, w1)
			}
			// A budget of 0 is not "no budget": HasSeverityBudget tells them apart.
			if got.MaxErrors == nil || *got.MaxErrors != 0 || got.MaxWarnings == nil || *got.MaxWarnings != 5 || got.MaxInfo != nil {
				t.Errorf("%s: budgets = %v/%v/%v, want 0/5/none", label, ptrInt(got.MaxErrors), ptrInt(got.MaxWarnings), ptrInt(got.MaxInfo))
			}
			if len(got.Rules) != 1 || got.Rules[0].MaxCount == nil || *got.Rules[0].MaxCount != 3 {
				t.Errorf("%s: rules = %+v, want todo-comment with a count of 3", label, got.Rules)
			}
		}
		got, err := b.GetPolicy(ctx, org, p.ID)
		if err != nil {
			t.Fatalf("GetPolicy: %v", err)
		}
		check("GetPolicy", got)
		got.Waivers[0].Reason = "changed by a reader"
		*got.MaxErrors = 9
		list, err := b.ListPolicies(ctx, org)
		if err != nil || len(list) != 1 {
			t.Fatalf("ListPolicies = %+v, %v; want the one policy", list, err)
		}
		check("ListPolicies", list[0])

		// Saving without them clears them.
		one := 1
		p.Waivers, p.MaxErrors, p.MaxWarnings, p.MaxInfo = nil, nil, nil, &one
		if err := b.SavePolicy(ctx, p); err != nil {
			t.Fatalf("SavePolicy (update): %v", err)
		}
		again, err := b.GetPolicy(ctx, org, p.ID)
		if err != nil {
			t.Fatalf("GetPolicy after the update: %v", err)
		}
		if len(again.Waivers) != 0 || again.MaxErrors != nil || again.MaxWarnings != nil || again.MaxInfo == nil || *again.MaxInfo != 1 {
			t.Errorf("after the update: waivers %+v, budgets %v/%v/%v; want none, none/none/1",
				again.Waivers, ptrInt(again.MaxErrors), ptrInt(again.MaxWarnings), ptrInt(again.MaxInfo))
		}
	})
}

// ptrInt renders an optional count for a failure message.
func ptrInt(p *int) any {
	if p == nil {
		return "none"
	}
	return *p
}
