package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// drainAudit collects the rows recorded so far.
func drainAudit(ch chan *storageif.AuditEvent) []*storageif.AuditEvent {
	var out []*storageif.AuditEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

// TestChatFix_ApprovedApplyIsAudited (R-security-4): approving an apply_fix in chat on an org flow records the
// decision and the write, both under the stream's owner and the flow's organisation, tied by stream and proposal.
func TestChatFix_ApprovedApplyIsAudited(t *testing.T) {
	doc := fixTestDoc()
	doc.OrganizationID = "org-1"
	svc, _ := newFixTestService(doc, func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		return commentDoc(d.ID), nil
	})
	ch := make(chan *storageif.AuditEvent, 8)
	svc.audit = audit.NewCapture(ch)
	ap := svc.newFixApplier(doc, "stream-1", (&fixEventRecorder{}).emit, nil, nil)
	prop := fixTestProposal()
	approveWhenShown(svc, prop.ProposalID, nil)
	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u-owner", Email: "owner@acme.io"})

	if _, err := ap.ApplyFixWithApproval(ctx, prop); err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	rows := drainAudit(ch)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d (%+v), want the decision and the write", len(rows), rows)
	}
	want := map[string]map[string]string{
		audit.ActionChatFixDecision: {"status": "approved"},
		audit.ActionFlowFixApply:    {"source": "chat", "outcome": "applied", "unsafe": "false", "blockId": "b1"},
	}
	for _, ev := range rows {
		w, ok := want[ev.Action]
		if !ok {
			t.Errorf("unexpected audit action %q", ev.Action)
			continue
		}
		if ev.UserID != "u-owner" || ev.OrgID != "org-1" || ev.ResourceType != "flow" || ev.ResourceID != doc.ID {
			t.Errorf("%s: user %q org %q resource %s/%s", ev.Action, ev.UserID, ev.OrgID, ev.ResourceType, ev.ResourceID)
		}
		if ev.Meta["streamId"] != "stream-1" || ev.Meta["proposalId"] != prop.ProposalID || ev.Meta["ruleId"] != prop.RuleID || ev.Meta["fixType"] != prop.FixType {
			t.Errorf("%s: meta %v, want the stream, proposal, rule and fix type", ev.Action, ev.Meta)
		}
		for k, v := range w {
			if ev.Meta[k] != v {
				t.Errorf("%s: meta[%s] = %q, want %q", ev.Action, k, ev.Meta[k], v)
			}
		}
	}
}

// TestChatFix_DeclineAndBatchAreAudited: a declined card records only its decision; an approved batch records the
// decision with the deselected count and one write row with the applied count.
func TestChatFix_DeclineAndBatchAreAudited(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		return commentDoc(d.ID), nil
	})
	ch := make(chan *storageif.AuditEvent, 8)
	svc.audit = audit.NewCapture(ch)
	ap := svc.newFixApplier(doc, "stream-1", (&fixEventRecorder{}).emit, nil, nil)
	prop := fixTestProposal()
	go func() {
		waitPending(t, svc, prop.ProposalID)
		_ = svc.ResolveFixDecision("stream-1", prop.ProposalID, false, nil)
	}()
	if _, err := ap.ApplyFixWithApproval(context.Background(), prop); err != nil {
		t.Fatal(err)
	}
	rows := drainAudit(ch)
	if len(rows) != 1 || rows[0].Action != audit.ActionChatFixDecision || rows[0].Meta["status"] != "declined" {
		t.Fatalf("decline rows = %+v, want one declined decision", rows)
	}

	second := fixTestProposal()
	second.ProposalID = "prop-2"
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			done := false
			svc.pendingFixDecisions.Range(func(k, _ any) bool {
				_ = svc.ResolveFixDecision("stream-1", k.(string), true, []int{1})
				done = true
				return false
			})
			if done {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if _, err := ap.ApplyFixesWithApproval(context.Background(), []ai.FixProposal{prop, second}); err != nil {
		t.Fatal(err)
	}
	rows = drainAudit(ch)
	if len(rows) != 2 {
		t.Fatalf("batch rows = %+v, want the decision and the write", rows)
	}
	if rows[0].Action != audit.ActionChatFixDecision || rows[0].Meta["status"] != "approved" || rows[0].Meta["excludedCount"] != "1" || rows[0].Meta["count"] != "2" {
		t.Errorf("batch decision row = %+v", rows[0])
	}
	if rows[1].Action != audit.ActionFlowFixApply || rows[1].Meta["applied"] != "1" || rows[1].Meta["source"] != "chat" {
		t.Errorf("batch write row = %+v", rows[1])
	}
}
