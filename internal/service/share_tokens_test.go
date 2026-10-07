package service

import (
	"context"
	"errors"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestShareTokenService_CreateResolveRevoke(t *testing.T) {
	ctx := context.Background()
	b := testutil.NewFakeBackend()
	b.Flows["f1"] = &storageif.FlowDocument{ID: "f1", Name: "Invoices", Content: []byte(`{}`)}
	svc := NewShareTokenService(b, b)

	tok, raw, err := svc.Create(ctx, "f1", "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(raw) != 64 {
		t.Errorf("raw token length = %d, want 64 hex chars", len(raw))
	}
	if tok.TokenHash == raw || tok.TokenHash != hashShareToken(raw) {
		t.Errorf("stored hash = %q, want the SHA-256 of the raw token", tok.TokenHash)
	}
	if tok.FlowID != "f1" || tok.CreatedBy != "alice" {
		t.Errorf("token = %+v", tok)
	}
	want := time.Now().AddDate(0, 0, 30)
	if tok.ExpiresAt == nil || tok.ExpiresAt.Sub(want).Abs() > time.Minute {
		t.Errorf("expiry = %v, want about %v", tok.ExpiresAt, want)
	}

	st, err := svc.Resolve(ctx, raw)
	if err != nil || st.FlowID != "f1" {
		t.Fatalf("resolve = %+v, %v", st, err)
	}
	if _, err := svc.Resolve(ctx, tok.TokenHash); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("resolving the hash itself = %v, want ErrNotFound", err)
	}
	if doc := svc.SharedFlowHeader(ctx, raw); doc == nil || doc.Name != "Invoices" || doc.Content != nil {
		t.Errorf("shared header = %+v, want the flow's header without content", doc)
	}
	if doc := svc.SharedFlowHeader(ctx, "unknown"); doc != nil {
		t.Errorf("unknown token header = %+v, want nil", doc)
	}

	list, err := svc.List(ctx, "f1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := svc.Revoke(ctx, "f1", list[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if doc := svc.SharedFlowHeader(ctx, raw); doc != nil {
		t.Errorf("revoked token header = %+v, want nil", doc)
	}
}

func TestShareTokenService_SharedFlowHeaderMissingFlow(t *testing.T) {
	ctx := context.Background()
	b := testutil.NewFakeBackend()
	svc := NewShareTokenService(b, b)
	_, raw, err := svc.Create(ctx, "gone", "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc := svc.SharedFlowHeader(ctx, raw); doc != nil {
		t.Errorf("header of a deleted flow = %+v, want nil", doc)
	}
}
