package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
	"pad-analyzer/internal/storage/contract"
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

// TestE2E_ContractSuite runs the full cross-backend contract suite against a
// blob-enabled backend, so blob offloading can't drift from the semantics the
// plain-Postgres and filesystem backends guarantee.
func TestE2E_ContractSuite(t *testing.T) {
	contract.RunSuite(t, newE2EBackend(t))
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

func TestE2E_A3_SaveCleansSupersededContentBlob(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()

	// Make the post-commit cleanup fire immediately instead of after the
	// reader-grace delay.
	old := staleBlobCleanupDelay
	staleBlobCleanupDelay = 0
	t.Cleanup(func() { staleBlobCleanupDelay = old })

	flow := &interfaces.FlowDocument{
		ID:       "e2e-a3-" + uuid.NewString(),
		Name:     "A3",
		Content:  json.RawMessage(`{"v":1}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 1},
		OwnerID:  "e2e-owner-" + uuid.NewString(),
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow (initial): %v", err)
	}
	supersededKey := flowContentKey(flow.ID, flow.Version)

	flow.Content = json.RawMessage(`{"v":2}`)
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow (update): %v", err)
	}

	// The new version's content must be live immediately.
	got, err := b.LoadFlow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if string(got.Content) != `{"v":2}` {
		t.Errorf("LoadFlow content mismatch after update: %s", got.Content)
	}

	// The previous version's blob is deleted by a detached post-commit
	// goroutine; poll until it is gone (downloadBlob maps 404 to nil, nil).
	deadline := time.Now().Add(15 * time.Second)
	for {
		content, err := b.downloadBlob(ctx, supersededKey)
		if err != nil {
			t.Fatalf("probe superseded blob: %v", err)
		}
		if content == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("superseded blob %q not removed within timeout", supersededKey)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestE2E_A5_VersionSnapshotCapPrunesRowsAndBlobs(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()

	oldCap := maxFlowVersionSnapshots
	maxFlowVersionSnapshots = 3
	oldDelay := staleBlobCleanupDelay
	staleBlobCleanupDelay = 0
	t.Cleanup(func() {
		maxFlowVersionSnapshots = oldCap
		staleBlobCleanupDelay = oldDelay
	})

	flow := &interfaces.FlowDocument{
		ID:      "e2e-a5-" + uuid.NewString(),
		Name:    "A5",
		Content: json.RawMessage(`{"k":"v"}`),
		OwnerID: "e2e-owner-" + uuid.NewString(),
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	for i := range 5 {
		fv := &interfaces.FlowVersion{
			ID: uuid.NewString(), FlowID: flow.ID,
			Content: json.RawMessage(`{"snap":true}`), CreatedBy: "e2e",
			CreatedAt: time.Now().UTC(),
		}
		if err := b.SaveFlowVersion(ctx, fv); err != nil {
			t.Fatalf("SaveFlowVersion %d: %v", i, err)
		}
	}

	// Only the newest 3 snapshots (versions 3, 4, 5) survive.
	versions, err := b.ListFlowVersions(ctx, flow.ID, 0)
	if err != nil {
		t.Fatalf("ListFlowVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 retained snapshots, got %d", len(versions))
	}
	if versions[0].Version != 5 || versions[2].Version != 3 {
		t.Errorf("expected versions 5..3 retained, got %d..%d", versions[0].Version, versions[2].Version)
	}
	// Retained snapshots stay loadable (their blobs must not be pruned).
	if _, err := b.LoadFlowVersion(ctx, flow.ID, 3); err != nil {
		t.Errorf("LoadFlowVersion(3) should survive the prune: %v", err)
	}

	// Pruned snapshots' blobs are removed by detached post-commit goroutines.
	deadline := time.Now().Add(15 * time.Second)
	for _, ver := range []int{1, 2} {
		key := fmt.Sprintf("flows/%s/versions/%d/content.json", flow.ID, ver)
		for {
			content, err := b.downloadBlob(ctx, key)
			if err != nil {
				t.Fatalf("probe pruned snapshot blob v%d: %v", ver, err)
			}
			if content == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pruned snapshot blob %q not removed within timeout", key)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// TestE2E_A6_ListFlowsMissingContentIsNilNotEmptyDoc verifies the fail-safe
// semantics the governance scanner and the GDPR export both depend on: when a
// flow's content blob is permanently gone, a full-content ListFlows surfaces it
// as nil content (so the scanner skips it rather than recording a bogus "empty"
// analysis), and ExportUserData refuses to produce an incomplete export.
func TestE2E_A6_ListFlowsMissingContentIsNilNotEmptyDoc(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	owner := "e2e-a6-owner-" + uuid.NewString()
	if err := b.CreateUser(ctx, &interfaces.User{
		ID: owner, Email: "a6-" + uuid.NewString() + "@example.com", Password: "h",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	flow := &interfaces.FlowDocument{
		ID:       "e2e-a6-" + uuid.NewString(),
		Name:     "A6",
		Content:  json.RawMessage(`{"blocks":[{"id":"a"}]}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 1},
		OwnerID:  owner,
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	// Delete the content blob out-of-band to simulate permanent loss.
	raw := e2eRawClient(t)
	if _, err := raw.DeleteBlob(ctx, b.container, flowContentKey(flow.ID, flow.Version), nil); err != nil {
		t.Fatalf("delete content blob: %v", err)
	}

	list, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, Limit: 100})
	if err != nil {
		t.Fatalf("ListFlows must not fail on a permanently-missing blob: %v", err)
	}
	found := findFlowByID(list, flow.ID)
	if found == nil {
		t.Fatalf("flow %s absent from list", flow.ID)
	}
	if len(found.Content) != 0 {
		t.Errorf("expected nil content for missing blob, got %q", found.Content)
	}

	// The export must refuse rather than ship the flow without content.
	if _, err := b.ExportUserData(ctx, owner); err == nil {
		t.Error("ExportUserData: expected refusal for missing content, got nil")
	}
	var buf bytes.Buffer
	if err := b.ExportUserDataTo(ctx, owner, &buf); err == nil {
		t.Error("ExportUserDataTo: expected refusal for missing content, got nil")
	}
}

// TestE2E_A7_StreamingExportMatchesBuffered verifies the streaming export
// produces valid JSON that round-trips to the same shape as the buffered
// ExportUserData, including blob-offloaded flow content.
func TestE2E_A7_StreamingExportMatchesBuffered(t *testing.T) {
	b := newE2EBackend(t)
	ctx := context.Background()
	owner := "e2e-a7-owner-" + uuid.NewString()
	if err := b.CreateUser(ctx, &interfaces.User{
		ID: owner, Email: "a7-" + uuid.NewString() + "@example.com", Password: "h",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	contents := map[string]string{}
	for i := range 3 {
		f := &interfaces.FlowDocument{
			ID:       fmt.Sprintf("e2e-a7-%d-%s", i, uuid.NewString()),
			Name:     fmt.Sprintf("A7-%d", i),
			Content:  json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			Metadata: interfaces.FlowMetadata{BlockCount: 1},
			OwnerID:  owner,
		}
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %d: %v", i, err)
		}
		contents[f.ID] = fmt.Sprintf(`{"i":%d}`, i)
	}

	var buf bytes.Buffer
	if err := b.ExportUserDataTo(ctx, owner, &buf); err != nil {
		t.Fatalf("ExportUserDataTo: %v", err)
	}

	var got interfaces.UserDataExport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("streamed export is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.User == nil || got.User.ID != owner {
		t.Fatalf("export user mismatch: %+v", got.User)
	}
	if len(got.Flows) != 3 {
		t.Fatalf("expected 3 flows in export, got %d", len(got.Flows))
	}
	for _, f := range got.Flows {
		want, ok := contents[f.ID]
		if !ok {
			t.Errorf("unexpected flow %s in export", f.ID)
			continue
		}
		if string(f.Content) != want {
			t.Errorf("flow %s content: got %s want %s", f.ID, f.Content, want)
		}
	}
	if got.ExportedAt.IsZero() {
		t.Error("export ExportedAt not set")
	}
}

func findFlowByID(flows []*interfaces.FlowDocument, id string) *interfaces.FlowDocument {
	for _, f := range flows {
		if f.ID == id {
			return f
		}
	}
	return nil
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
	for b.countBlobs(t, prefix) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("blobs under %q not removed within timeout", prefix)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
