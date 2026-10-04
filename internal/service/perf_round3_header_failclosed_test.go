package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestGetAuthorizedHeader_FailsClosedWithoutAuthz: cloud storage with no
// authorizer is a wiring error. The header path used to fall through to a
// bare LoadFlowHeader there (fail open) while CheckFlowPermission denied; the
// two editor-gated writes that moved onto the header path must stay closed.
func TestGetAuthorizedHeader_FailsClosedWithoutAuthz(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedCloudFlow(t, backend, "f1")
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil /* no authz */, nil)

	hdr, err := svc.GetAuthorizedHeader(context.Background(), "f1", "u1", "editor")
	if !errors.Is(err, ErrPermissionDenied) || hdr != nil {
		t.Fatalf("GetAuthorizedHeader without an authorizer = (%+v, %v), want ErrPermissionDenied", hdr, err)
	}
}
