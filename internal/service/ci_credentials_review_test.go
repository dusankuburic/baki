package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/hmacsig"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// headerFailingStore counts the fallback's flow reads and fails them all, as Postgres does for a flow id with a NUL.
type headerFailingStore struct {
	*testutil.FakeBackend
	reads atomic.Int32
}

func (s *headerFailingStore) LoadFlowHeader(context.Context, string) (*storageif.FlowDocument, error) {
	s.reads.Add(1)
	return nil, errors.New(`scan flow header: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)`)
}

// Before authentication the old-client fallback answers every failure, a storage error included, with the caller's
// 401: a body whose flowId Postgres refuses made the webhook a 500 with an ERROR log (a log and alert flood).
func TestCIService_FallbackStoreErrorIsUnauthenticated(t *testing.T) {
	store := &headerFailingStore{FakeBackend: testutil.NewFakeBackend()}
	svc := NewCIService(store, NewCloudDocumentProvider(store), nil, CIOptions{OrgCredentials: true})
	_, err := svc.Authenticate(context.Background(), signed("not-a-credential", "", "flow-x"))
	if _, typed := apperr.As(err); !typed || !errors.Is(err, ErrCIUnauthenticated) {
		t.Fatalf("Authenticate = %v, want ErrCIUnauthenticated", err)
	}
	if store.reads.Load() != 1 {
		t.Errorf("the plausible flow id was read %d times, want 1", store.reads.Load())
	}
}

// A flow id that cannot name a flow, and a body larger than any old client sends, are not looked up at all: no
// storage read (and no per-credential HMAC of a large body) for an unauthenticated caller.
func TestCIService_FallbackSkipsImplausibleCalls(t *testing.T) {
	store := &headerFailingStore{FakeBackend: testutil.NewFakeBackend()}
	svc := NewCIService(store, NewCloudDocumentProvider(store), nil, CIOptions{OrgCredentials: true})
	for name, req := range map[string]CIRequest{
		"NUL":        signed("k", "", "x\\u0000y"),
		"control":    signed("k", "", "x\\ny"),
		"long":       signed("k", "", strings.Repeat("f", 129)),
		"large body": bigCall(maxFallbackBody + 1),
		"no flow id": signed("k", "", ""),
		"not JSON":   {Timestamp: strconv.FormatInt(time.Now().Unix(), 10), Signature: "sha256=00", Body: []byte("flowId=x")},
	} {
		if _, err := svc.Authenticate(context.Background(), req); !errors.Is(err, ErrCIUnauthenticated) {
			t.Errorf("%s: Authenticate = %v, want ErrCIUnauthenticated", name, err)
		}
	}
	if n := store.reads.Load(); n != 0 {
		t.Errorf("implausible calls read %d flow headers, want 0", n)
	}
}

// bigCall is a signed call whose body pads a valid flow id past n bytes.
func bigCall(n int) CIRequest {
	body := []byte(`{"flowId":"flow-a","pad":"` + strings.Repeat("x", n) + `"}`)
	ts := time.Now().Unix()
	return CIRequest{Timestamp: strconv.FormatInt(ts, 10), Signature: hmacsig.Sign([]byte("k"), ts, body), Body: body}
}
