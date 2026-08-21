package padcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"pad-core/models"
)

// mockClient/Converter/Store let the Ingester orchestration be tested without
// the Power Platform API, the format bridge, or the DB. The mocks are
// concurrency-safe: Ingest processes flows through a worker pool.
type mockClient struct {
	flows []DesktopFlowRef
	defs  map[string]json.RawMessage // flowID → definition
	err   error                      // list error (if any)
}

func (m *mockClient) ListDesktopFlows(ctx context.Context) ([]DesktopFlowRef, error) {
	return m.flows, m.err
}
func (m *mockClient) GetFlowDefinition(ctx context.Context, flowID string) (json.RawMessage, error) {
	if d, ok := m.defs[flowID]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

type mockConverter struct {
	docs  map[string]*models.FlowDocument // flowID → doc (by name lookup)
	errOn string                          // name that errors
}

func (m *mockConverter) Convert(name string, def json.RawMessage) (*models.FlowDocument, error) {
	if name == m.errOn {
		return nil, errors.New("convert failed")
	}
	if d, ok := m.docs[name]; ok {
		return d, nil
	}
	return nil, nil // skip when unknown
}

type mockStore struct {
	mu       sync.Mutex
	upserted map[string]string // sourceID → doc name
	errOn    string            // sourceID that errors
}

func (m *mockStore) UpsertFlow(ctx context.Context, doc *models.FlowDocument, sourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upserted == nil {
		m.upserted = map[string]string{}
	}
	if sourceID == m.errOn {
		return errors.New("store failed")
	}
	m.upserted[sourceID] = doc.Name
	return nil
}

// TestIngester_HappyPath confirms the full orchestration: list → fetch →
// convert → store, counting ingested correctly.
func TestIngester_HappyPath(t *testing.T) {
	flows := []DesktopFlowRef{{ID: "f1", Name: "Flow A"}, {ID: "f2", Name: "Flow B"}}
	client := &mockClient{flows: flows, defs: map[string]json.RawMessage{"f1": []byte("a"), "f2": []byte("b")}}
	converter := &mockConverter{docs: map[string]*models.FlowDocument{"Flow A": {Name: "Flow A"}, "Flow B": {Name: "Flow B"}}}
	store := &mockStore{}

	res, err := NewIngester(client, converter, store).Ingest(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Ingested != 2 || res.Failed != 0 {
		t.Errorf("ingested=%d failed=%d, want 2/0", res.Ingested, res.Failed)
	}
	if len(store.upserted) != 2 || store.upserted["f1"] != "Flow A" {
		t.Errorf("store calls = %v, want f1→Flow A + f2→Flow B", store.upserted)
	}
}

// TestIngester_PartialFailureContinues verifies a single bad flow (convert or
// store error) doesn't abort the batch — the rest still ingest, errors collected.
func TestIngester_PartialFailureContinues(t *testing.T) {
	flows := []DesktopFlowRef{{ID: "f1", Name: "Good"}, {ID: "f2", Name: "Bad"}, {ID: "f3", Name: "AlsoGood"}}
	client := &mockClient{flows: flows, defs: map[string]json.RawMessage{"f1": []byte("a"), "f2": []byte("b"), "f3": []byte("c")}}
	converter := &mockConverter{errOn: "Bad", docs: map[string]*models.FlowDocument{"Good": {Name: "Good"}, "AlsoGood": {Name: "AlsoGood"}}}
	store := &mockStore{}

	res, err := NewIngester(client, converter, store).Ingest(context.Background())
	if err != nil {
		t.Fatalf("unexpected batch-level error: %v", err)
	}
	if res.Ingested != 2 || res.Failed != 1 {
		t.Errorf("ingested=%d failed=%d, want 2/1", res.Ingested, res.Failed)
	}
	if len(res.Errors) != 1 || res.Errors[0] == "" {
		t.Errorf("expected 1 non-empty error, got %v", res.Errors)
	}
}

// TestIngester_StoreFailureIsRecorded confirms a store error on one flow is
// counted as failed (not ingested) and doesn't block the rest.
func TestIngester_StoreFailureIsRecorded(t *testing.T) {
	flows := []DesktopFlowRef{{ID: "f1", Name: "A"}, {ID: "f2", Name: "B"}}
	client := &mockClient{flows: flows, defs: map[string]json.RawMessage{"f1": []byte("a"), "f2": []byte("b")}}
	converter := &mockConverter{docs: map[string]*models.FlowDocument{"A": {Name: "A"}, "B": {Name: "B"}}}
	store := &mockStore{errOn: "f1"}

	res, _ := NewIngester(client, converter, store).Ingest(context.Background())
	if res.Ingested != 1 || res.Failed != 1 {
		t.Errorf("ingested=%d failed=%d, want 1/1 (f1 store error)", res.Ingested, res.Failed)
	}
}

// TestIngester_ListErrorAborts confirms a list-level error aborts the pass
// (returns the error) rather than producing a misleading zero-result success.
func TestIngester_ListErrorAborts(t *testing.T) {
	client := &mockClient{err: errors.New("401 unauthorized")}
	res, err := NewIngester(client, &mockConverter{}, &mockStore{}).Ingest(context.Background())
	if err == nil {
		t.Fatal("expected a list-level error to abort, got nil")
	}
	if res.Ingested != 0 {
		t.Errorf("expected 0 ingested on list error, got %d", res.Ingested)
	}
}

// panickingConverter wraps the happy-path conversion but panics on a named flow,
// simulating a nil-deref / stack-overflow in the cloud-format bridge.
type panickingConverter struct {
	docs    map[string]*models.FlowDocument
	panicOn string
}

func (p *panickingConverter) Convert(name string, def json.RawMessage) (*models.FlowDocument, error) {
	if name == p.panicOn {
		panic("simulated converter panic")
	}
	if d, ok := p.docs[name]; ok {
		return d, nil
	}
	return nil, nil
}

// TestIngester_ConverterPanicContinues verifies a PANIC on one flow (not just a
// returned error) is caught per-flow and the batch continues. Without the
// per-flow recover, a single panicking flow unwinds the whole Ingest call and
// every subsequent flow in the batch is skipped.
func TestIngester_ConverterPanicContinues(t *testing.T) {
	flows := []DesktopFlowRef{{ID: "f1", Name: "Good"}, {ID: "f2", Name: "Panics"}, {ID: "f3", Name: "AlsoGood"}}
	client := &mockClient{
		flows: flows,
		defs:  map[string]json.RawMessage{"f1": []byte("a"), "f2": []byte("b"), "f3": []byte("c")},
	}
	converter := &panickingConverter{
		panicOn: "Panics",
		docs:    map[string]*models.FlowDocument{"Good": {Name: "Good"}, "AlsoGood": {Name: "AlsoGood"}},
	}
	store := &mockStore{}

	res, err := NewIngester(client, converter, store).Ingest(context.Background())
	if err != nil {
		t.Fatalf("unexpected batch-level error: %v", err)
	}
	if res.Ingested != 2 || res.Failed != 1 {
		t.Errorf("ingested=%d failed=%d, want 2/1 (panicking flow recorded, rest continue)", res.Ingested, res.Failed)
	}
	if len(res.Errors) != 1 || res.Errors[0] == "" {
		t.Errorf("expected 1 non-empty panic error, got %v", res.Errors)
	}
}

// TestIngester_LargeBatchUnderWorkerPool exercises the parallel path (>6 flows
// fills every worker) and proves: exact ingested/failed counts under
// concurrency, the lastModified map is correct (second sweep skips everything),
// and pruning drops entries for flows the environment no longer returns.
func TestIngester_LargeBatchUnderWorkerPool(t *testing.T) {
	const n = 40
	mod := time.Now()
	flows := make([]DesktopFlowRef, 0, n)
	defs := make(map[string]json.RawMessage, n)
	docs := make(map[string]*models.FlowDocument, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("f%03d", i)
		name := fmt.Sprintf("Flow %03d", i)
		flows = append(flows, DesktopFlowRef{ID: id, Name: name, Modified: mod})
		defs[id] = []byte("def")
		docs[name] = &models.FlowDocument{Name: name}
	}
	ing := NewIngester(&mockClient{flows: flows, defs: defs}, &mockConverter{docs: docs}, &mockStore{})

	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Ingested != n || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("first sweep: ingested=%d failed=%d skipped=%d, want %d/0/0", res.Ingested, res.Failed, res.Skipped, n)
	}
	if len(ing.lastModified) != n {
		t.Fatalf("lastModified has %d entries, want %d", len(ing.lastModified), n)
	}

	// Second sweep over the same set: all skipped, no fetches.
	res2, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if res2.Skipped != n || res2.Ingested != 0 {
		t.Fatalf("steady-state sweep: skipped=%d ingested=%d, want %d/0", res2.Skipped, res2.Ingested, n)
	}

	// Third sweep with only 5 flows remaining: the pruned map must not retain
	// the 35 vanished entries.
	ing.client = &mockClient{flows: flows[:5], defs: defs}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("prune ingest: %v", err)
	}
	if len(ing.lastModified) != 5 {
		t.Errorf("lastModified after prune has %d entries, want 5 (vanished flows dropped)", len(ing.lastModified))
	}
}
