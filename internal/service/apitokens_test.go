package service

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestAPITokenService_Lifecycle(t *testing.T) {
	ctx := context.Background()
	svc := NewAPITokenService(testutil.NewFakeBackend())

	if tokens, err := svc.List(ctx, "u1"); err != nil || tokens == nil || len(tokens) != 0 {
		t.Errorf("empty List = %v, %v; want [] (never nil)", tokens, err)
	}
	if err := svc.Create(ctx, &storageif.APIToken{ID: "t1", UserID: "u1", TokenHash: "h1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, "u2", "t1"); err != nil {
		t.Errorf("Delete by another user: %v", err)
	}
	if tokens, _ := svc.List(ctx, "u1"); len(tokens) != 1 {
		t.Errorf("another user revoked the token: %v", tokens)
	}
	if err := svc.Delete(ctx, "u1", "t1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if tokens, _ := svc.List(ctx, "u1"); len(tokens) != 0 {
		t.Errorf("token survived Delete: %v", tokens)
	}
}
