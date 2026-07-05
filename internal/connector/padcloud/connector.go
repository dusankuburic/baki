package padcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

// DesktopFlowRef identifies a desktop flow in a Power Platform environment.
type DesktopFlowRef struct {
	ID       string    // the environment-unique flow id (used as the source key)
	Name     string    // display name
	Solution string    // solution it belongs to (for grouping)
	Modified time.Time // last modified (for incremental pulls)
}

// Client enumerates desktop flows in an environment and fetches a flow's
// definition (the cloud action-tree JSON). The HTTP implementation's endpoints
// are documented for validation against a real tenant; the interface keeps the
// ingester independent of the wire shape.
type Client interface {
	ListDesktopFlows(ctx context.Context) ([]DesktopFlowRef, error)
	GetFlowDefinition(ctx context.Context, flowID string) (json.RawMessage, error)
}

// Converter is the format bridge: it turns a PAD cloud flow definition (JSON
// action tree) into the parser's models.FlowDocument — the representation the
// analyzer, library, and governance surface consume.
//
// THIS is the de-risking interface for Phase 4. PAD's cloud action schema
// (nested "actions"/"rpaActions" with type+properties+nestedItems) differs from
// the .txt export grammar the parser reads; a correct converter must be
// validated against a real API response. The interface lets the concrete impl
// evolve (and be unit-tested with fixtures) without touching the ingester.
type Converter interface {
	Convert(name string, def json.RawMessage) (*models.FlowDocument, error)
}

// Store persists an ingested flow into the library, keyed by its Power Platform
// source id so re-ingest updates in place rather than duplicating. Decoupled
// from the DB so the ingester is testable without storage.
type Store interface {
	UpsertFlow(ctx context.Context, doc *models.FlowDocument, sourceID string) error
}

// IngestResult summarises one ingest pass.
type IngestResult struct {
	Ingested int      // flows successfully converted + stored
	Failed   int      // flows that errored (errors collected below)
	Errors   []string // per-flow error summaries (for alerting/logging)
	Skipped  int      // flows skipped (e.g. convert returned nil without error)
}

// Ingester orchestrates one ingest pass: list desktop flows → for each, fetch
// its definition → convert to a FlowDocument → store. It is agnostic of the
// auth/client/converter/store implementations (all injected), which makes the
// orchestration — the part that most often has subtle bugs (partial failure,
// one bad flow aborting the batch) — directly unit-testable with mocks.
type Ingester struct {
	client    Client
	converter Converter
	store     Store
}

// NewIngester wires the collaborator implementations.
func NewIngester(client Client, converter Converter, store Store) *Ingester {
	return &Ingester{client: client, converter: converter, store: store}
}

// Ingest runs one full pass. A failure on a single flow (fetch/convert/store)
// is recorded and the batch continues — so one malformed flow doesn't block the
// rest of the environment. Returns an error only if listing itself fails.
func (i *Ingester) Ingest(ctx context.Context) (IngestResult, error) {
	var res IngestResult

	flows, err := i.client.ListDesktopFlows(ctx)
	if err != nil {
		return res, fmt.Errorf("list desktop flows: %w", err)
	}

	for _, f := range flows {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		def, err := i.client.GetFlowDefinition(ctx, f.ID)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): fetch definition: %v", f.Name, f.ID, err))
			continue
		}
		doc, err := i.converter.Convert(f.Name, def)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): convert: %v", f.Name, f.ID, err))
			continue
		}
		if doc == nil {
			res.Skipped++
			continue
		}
		if err := i.store.UpsertFlow(ctx, doc, f.ID); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): store: %v", f.Name, f.ID, err))
			continue
		}
		res.Ingested++
	}

	logger.Info("padcloud ingest complete",
		"listed", len(flows), "ingested", res.Ingested, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}
