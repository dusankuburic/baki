package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
)

// azuriteDefaultConnStr is Azurite's well-known development connection string.
// It is only used for tests that never touch the network (the size-cap guard
// rejects before any client call), so a live emulator is not required.
const azuriteDefaultConnStr = "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;" +
	"AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;" +
	"BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

// --- Pure unit tests (always run, no emulator) ---

func TestFlowContentKey(t *testing.T) {
	if got := flowContentKey("abc", 7); got != "flows/abc/content.v7.json" {
		t.Errorf("flowContentKey = %q, want flows/abc/content.v7.json", got)
	}
	if got := legacyFlowContentKey("abc"); got != "flows/abc/content.json" {
		t.Errorf("legacyFlowContentKey = %q, want flows/abc/content.json", got)
	}
	// The versioned key must never collide with the snapshot namespace used by
	// SaveFlowVersion (flows/{id}/versions/{n}/content.json).
	if got := flowContentKey("abc", 3); got == "flows/abc/versions/3/content.json" {
		t.Errorf("flowContentKey collides with snapshot namespace: %q", got)
	}
}

func TestBlobErrStatus(t *testing.T) {
	if got := blobErrStatus(&azcore.ResponseError{StatusCode: 429}); got != "throttled" {
		t.Errorf("429 -> %q, want throttled", got)
	}
	if got := blobErrStatus(&azcore.ResponseError{StatusCode: 500}); got != "error" {
		t.Errorf("500 -> %q, want error", got)
	}
	if got := blobErrStatus(fmt.Errorf("boom")); got != "error" {
		t.Errorf("plain error -> %q, want error", got)
	}
}

func TestUploadBlobSizeCap(t *testing.T) {
	// Build a client from the well-known connection string. The size guard runs
	// before any network call, so this test stays hermetic — the emulator is
	// never contacted for the oversized payload (it is rejected first).
	client, err := azblob.NewClientFromConnectionString(azuriteDefaultConnStr, nil)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	b := &PostgresStorageBackend{blobClient: client, container: "cap-test"}

	err = b.uploadBlob(context.Background(), "flows/x/content.v1.json", make([]byte, maxBlobContentBytes+1))
	if err == nil {
		t.Fatal("expected size-cap error for oversized content, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size-cap error, got: %v", err)
	}
}

// --- Azurite-gated integration tests ---
//
// Set AZURITE_CONNECTION_STRING to run these against a live Azurite emulator,
// e.g. `AZURITE_CONNECTION_STRING="$azuriteDefaultConnStr" go test ./internal/storage/database/...`
// with `azurite-blob` listening on 127.0.0.1:10000.

// newAzuriteBackend builds a backend pointed at Azurite with a unique throwaway
// container, skipping the test when AZURITE_CONNECTION_STRING is unset or the
// emulator is unreachable.
func newAzuriteBackend(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	connStr := os.Getenv("AZURITE_CONNECTION_STRING")
	if connStr == "" {
		t.Skip("AZURITE_CONNECTION_STRING not set — skipping blob integration tests")
	}
	client, err := azblob.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		t.Fatalf("build azurite client: %v", err)
	}
	container := "test-" + uuid.NewString()
	if _, err := client.CreateContainer(context.Background(), container, nil); err != nil {
		t.Skipf("cannot create container on Azurite (emulator running?): %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteContainer(context.Background(), container, nil)
	})
	return &PostgresStorageBackend{blobClient: client, container: container}
}

func TestBlob_UploadDownloadRoundTrip(t *testing.T) {
	b := newAzuriteBackend(t)
	ctx := context.Background()
	key := flowContentKey("flow-1", 1)
	want := []byte(`{"blocks":[{"id":"a"}]}`)

	if err := b.uploadBlob(ctx, key, want); err != nil {
		t.Fatalf("uploadBlob: %v", err)
	}
	got, err := b.downloadBlob(ctx, key)
	if err != nil {
		t.Fatalf("downloadBlob: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}

func TestBlob_DownloadNotFound(t *testing.T) {
	b := newAzuriteBackend(t)
	got, err := b.downloadBlob(context.Background(), flowContentKey("missing", 9))
	if err != nil {
		t.Fatalf("downloadBlob of missing key should not error, got: %v", err)
	}
	if got != nil {
		t.Fatalf("downloadBlob of missing key should return nil, got %d bytes", len(got))
	}
}

func TestBlob_DeleteBlobsBatch(t *testing.T) {
	b := newAzuriteBackend(t)
	ctx := context.Background()

	// Upload more than one batch (>256) under a flow prefix to exercise the
	// chunked Blob Batch path, plus a few blobs under a sibling prefix that must
	// survive.
	flowID := uuid.NewString()
	prefix := fmt.Sprintf("flows/%s/", flowID)
	const n = 300
	for i := range n {
		key := fmt.Sprintf("%scontent.v%d.json", prefix, i)
		if err := b.uploadBlob(ctx, key, []byte("{}")); err != nil {
			t.Fatalf("uploadBlob %d: %v", i, err)
		}
	}
	const siblings = 3
	for i := range siblings {
		if err := b.uploadBlob(ctx, fmt.Sprintf("flows/other-%d/content.v1.json", i), []byte("{}")); err != nil {
			t.Fatalf("uploadBlob sibling %d: %v", i, err)
		}
	}

	if err := b.deleteBlobs(ctx, prefix); err != nil {
		t.Fatalf("deleteBlobs: %v", err)
	}

	if got := b.countBlobs(t, prefix); got != 0 {
		t.Fatalf("expected 0 blobs under %q after delete, got %d", prefix, got)
	}
	if got := b.countBlobs(t, "flows/other-"); got != siblings {
		t.Fatalf("sibling blobs were deleted: got %d want %d", got, siblings)
	}
}

func (b *PostgresStorageBackend) countBlobs(t *testing.T, prefix string) int {
	t.Helper()
	pager := b.blobClient.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	count := 0
	for pager.More() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatalf("list blobs: %v", err)
		}
		count += len(page.Segment.BlobItems)
	}
	return count
}
