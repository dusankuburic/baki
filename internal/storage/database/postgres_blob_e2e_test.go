package database

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
	"pad-analyzer/internal/storage/interfaces"
)

// newE2EBackend builds the full PostgresStorageBackend (real Postgres + Azurite
// blob) via New(), so the DB↔blob path is exercised exactly as in production.
// Skips unless both DATABASE_URL and AZURITE_CONNECTION_STRING are set. Each
// call uses a fresh throwaway container.
func newE2EBackend(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	connStr := os.Getenv("AZURITE_CONNECTION_STRING")
	if dsn == "" || connStr == "" {
		t.Skip("DATABASE_URL and AZURITE_CONNECTION_STRING must both be set — skipping DB+blob E2E tests")
	}

	// Pre-create the container so New()'s startup probe finds it.
	raw, err := azblob.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		t.Fatalf("build raw blob client: %v", err)
	}
	container := "e2e-" + uuid.NewString()
	if _, err := raw.CreateContainer(context.Background(), container, nil); err != nil {
		t.Skipf("cannot create container on Azurite (emulator running?): %v", err)
	}

	cfg := DefaultConfig(dsn)
	cfg.AzureBlobConnectionString = connStr
	cfg.AzureStorageContainer = container
	b, err := New(context.Background(), cfg)
	if err != nil {
		_, _ = raw.DeleteContainer(context.Background(), container, nil)
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = b.Close()
		_, _ = raw.DeleteContainer(context.Background(), container, nil)
	})
	return b
}

func e2eRawClient(t *testing.T) *azblob.Client {
	t.Helper()
	c, err := azblob.NewClientFromConnectionString(os.Getenv("AZURITE_CONNECTION_STRING"), nil)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	return c
}

func TestE2E_SaveLoadRoundTrip(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	owner := "e2e-owner-" + uuid.NewString()
	flow := &interfaces.FlowDocument{
		ID:       "e2e-flow-" + uuid.NewString(),
		Name:     "RoundTrip",
		Content:  json.RawMessage(`{"blocks":[{"id":"a"},{"id":"b"}]}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 2},
		OwnerID:  owner,
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	// DB row holds the placeholder; real content lives in the blob.
	var dbContent string
	if err := b.DB().QueryRowContext(ctx, `SELECT content::text FROM flows WHERE id=$1`, flow.ID).Scan(&dbContent); err != nil {
		t.Fatalf("read db content: %v", err)
	}
	if dbContent != "{}" {
		t.Errorf("expected DB placeholder {}, got %q", dbContent)
	}

	got, err := b.LoadFlow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if string(got.Content) != `{"blocks":[{"id":"a"},{"id":"b"}]}` {
		t.Errorf("LoadFlow content mismatch: %s", got.Content)
	}

	// Header path must not fetch content.
	hdr, err := b.LoadFlowHeader(ctx, flow.ID)
	if err != nil {
		t.Fatalf("LoadFlowHeader: %v", err)
	}
	if len(hdr.Content) != 0 {
		t.Errorf("LoadFlowHeader returned content: %s", hdr.Content)
	}
}

func TestE2E_A1_MissingContentBlobErrors(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	flow := &interfaces.FlowDocument{
		ID:       "e2e-a1-" + uuid.NewString(),
		Name:     "A1",
		Content:  json.RawMessage(`{"blocks":[{"id":"a"}]}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 1},
		OwnerID:  "e2e-owner-" + uuid.NewString(),
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	// Delete the content blob out-of-band to simulate loss/corruption.
	raw := e2eRawClient(t)
	if _, err := raw.DeleteBlob(ctx, b.container, flowContentKey(flow.ID, flow.Version), nil); err != nil {
		t.Fatalf("delete content blob: %v", err)
	}

	// LoadFlow must fail loudly (not return a hollow {}), because metadata says
	// the flow has blocks.
	if _, err := b.LoadFlow(ctx, flow.ID); err == nil {
		t.Fatal("LoadFlow: expected error for missing content blob, got nil")
	}
	// Authorization-style header load must still succeed (blast-radius fix #1).
	if _, err := b.LoadFlowHeader(ctx, flow.ID); err != nil {
		t.Fatalf("LoadFlowHeader should not depend on the content blob: %v", err)
	}
}

func TestE2E_A2_LegacyKeyFallback(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	flow := &interfaces.FlowDocument{
		ID:       "e2e-a2-" + uuid.NewString(),
		Name:     "A2",
		Content:  json.RawMessage(`{"v":"current"}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 1},
		OwnerID:  "e2e-owner-" + uuid.NewString(),
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	raw := e2eRawClient(t)
	// Simulate a pre-migration flow: remove the versioned key and write the
	// legacy version-agnostic key instead.
	if _, err := raw.DeleteBlob(ctx, b.container, flowContentKey(flow.ID, flow.Version), nil); err != nil {
		t.Fatalf("delete versioned blob: %v", err)
	}
	legacy := []byte(`{"v":"legacy"}`)
	if _, err := raw.UploadBuffer(ctx, b.container, legacyFlowContentKey(flow.ID), legacy, nil); err != nil {
		t.Fatalf("upload legacy blob: %v", err)
	}

	got, err := b.LoadFlow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if string(got.Content) != `{"v":"legacy"}` {
		t.Errorf("expected legacy-key fallback content, got %s", got.Content)
	}
}

func TestE2E_A4_DeleteRemovesAllBlobs(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	flow := &interfaces.FlowDocument{
		ID:       "e2e-a4-" + uuid.NewString(),
		Name:     "A4",
		Content:  json.RawMessage(`{"k":"v"}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 1},
		OwnerID:  "e2e-owner-" + uuid.NewString(),
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	// Add a couple of version snapshots so the prefix has multiple blobs.
	for v := 1; v <= 2; v++ {
		fv := &interfaces.FlowVersion{
			ID: uuid.NewString(), FlowID: flow.ID, Version: v,
			Content: json.RawMessage(`{"snap":true}`), CreatedBy: "e2e",
			CreatedAt: time.Now().UTC(),
		}
		if err := b.SaveFlowVersion(ctx, fv); err != nil {
			t.Fatalf("SaveFlowVersion: %v", err)
		}
	}

	prefix := "flows/" + flow.ID + "/"
	if b.countBlobs(t, prefix) == 0 {
		t.Fatal("expected blobs under prefix before delete")
	}
	if err := b.DeleteFlow(ctx, flow.ID); err != nil {
		t.Fatalf("DeleteFlow: %v", err)
	}
	// Cleanup runs as a post-commit goroutine (autocommit ⇒ fired immediately),
	// so poll until the prefix is empty.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if b.countBlobs(t, prefix) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blobs under %q not removed within timeout", prefix)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
