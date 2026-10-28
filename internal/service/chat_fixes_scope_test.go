package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-core/models"
)

// scopedCtx carries a scoped API token's claims the way jwtAuth installs them for a PAT; the chat stream keeps
// them because its context is derived with context.WithoutCancel.
func scopedCtx(scopes ...string) context.Context {
	return auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-1", Scopes: scopes})
}

// failingApplyHook fails the test if a fix is ever applied.
func failingApplyHook(t *testing.T) applyFixFunc {
	return func(_ context.Context, _ *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		t.Errorf("fix applied for block %q despite a token without the write scope", blockID)
		return nil, errors.New("must not apply")
	}
}

func TestRequireFixWriteScope(t *testing.T) {
	cases := []struct {
		name    string
		ctx     context.Context
		allowed bool
	}{
		{"no claims (local mode)", context.Background(), true},
		{"interactive session", auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-1"}), true},
		{"chat+write token", scopedCtx(auth.ScopeChat, auth.ScopeWrite), true},
		{"chat-only token", scopedCtx(auth.ScopeChat), false},
		{"chat+read token", scopedCtx(auth.ScopeChat, auth.ScopeRead), false},
	}
	for _, tc := range cases {
		err := requireFixWriteScope(tc.ctx)
		if tc.allowed && err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if !tc.allowed && !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("%s: err = %v, want ErrPermissionDenied", tc.name, err)
		}
	}
}

// TestApplyFix_ChatOnlyTokenRefusedBeforePrompt: a chat-only token may start a stream, but apply_fix and
// apply_fixes must answer the model with a refusal instead of prompting for an approval it could never act on.
func TestApplyFix_ChatOnlyTokenRefusedBeforePrompt(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, failingApplyHook(t))
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	ctx := scopedCtx(auth.ScopeChat)

	out, err := ap.ApplyFixWithApproval(ctx, fixTestProposal())
	if err != nil || out != fixScopeRefusal {
		t.Errorf("single fix: out=%q err=%v, want the scope refusal", out, err)
	}
	out, err = ap.ApplyFixesWithApproval(ctx, []ai.FixProposal{fixTestProposal()})
	if err != nil || out != fixScopeRefusal {
		t.Errorf("batch: out=%q err=%v, want the scope refusal", out, err)
	}
	if evs := rec.snapshot(); len(evs) != 0 {
		t.Errorf("no approval prompt may be emitted, got %v", evs)
	}
	svc.pendingFixDecisions.Range(func(k, _ any) bool {
		t.Errorf("pending decision registered: %v", k)
		return true
	})
}

// TestApplyApprovedFix_RechecksWriteScope: the apply step itself refuses, so no path to the mutation skips the check
// (failingApplyHook fails the test if the mutation runs). The refusal is an error outcome the model reads, not a
// stream error (R-approvals-2).
func TestApplyApprovedFix_RechecksWriteScope(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, failingApplyHook(t))
	ctx := scopedCtx(auth.ScopeChat)

	out, _, err := svc.applyApprovedFix(ctx, doc, fixTestProposal())
	if err != nil || out.Status != "error" || !strings.Contains(out.Detail, "write scope") {
		t.Errorf("applyApprovedFix = %+v, %v; want an error outcome naming the write scope", out, err)
	}
	batch, _, err := svc.applyApprovedFixes(ctx, doc, []ai.FixProposal{fixTestProposal()}, nil)
	if err != nil || batch.Status != "error" || !strings.Contains(batch.Detail, "write scope") {
		t.Errorf("applyApprovedFixes = %+v, %v; want an error outcome naming the write scope", batch, err)
	}
}
