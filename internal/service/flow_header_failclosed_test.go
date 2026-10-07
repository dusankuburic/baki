package service

import (
	"errors"
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestGetAuthorizedHeader_FailsClosedWithoutAuthz: cloud storage with no
// authorizer is a wiring error. The header path used to fall through to a
// bare LoadFlowHeader there (fail open) while CheckFlowPermission denied, and
// GetAuthorized resolved the document unchecked; every flow check must stay
// closed.
func TestGetAuthorizedHeader_FailsClosedWithoutAuthz(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedCloudFlow(t, backend, "f1")
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil /* no authz */, nil)
	ctx := actorCtx("u1")

	hdr, err := svc.GetAuthorizedHeader(ctx, "f1", FlowEdit)
	if !errors.Is(err, ErrPermissionDenied) || hdr != nil {
		t.Fatalf("GetAuthorizedHeader without an authorizer = (%+v, %v), want ErrPermissionDenied", hdr, err)
	}
	if doc, err := svc.GetAuthorized(ctx, "f1", FlowRead); !errors.Is(err, ErrPermissionDenied) || doc != nil {
		t.Errorf("GetAuthorized without an authorizer = (%v, %v), want ErrPermissionDenied", doc != nil, err)
	}
	if err := svc.CheckFlowPermission(ctx, "f1", FlowRead); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("CheckFlowPermission without an authorizer = %v, want ErrPermissionDenied", err)
	}
}
