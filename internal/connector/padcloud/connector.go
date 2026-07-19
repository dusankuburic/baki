package padcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"pad-core/logger"
	"pad-core/models"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/metrics"
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
//
// Start/Stop add an optional periodic loop over Ingest (mirroring the governance
// scanner's lifecycle), so the connector can be wired into the app lifecycle and
// left disabled (no-op) when not configured.
type Ingester struct {
	client    Client
	converter Converter
	store     Store

	// lastModified tracks the last-seen Modified timestamp per flow ID so
	// sweeps can skip GetFlowDefinition for unchanged flows (saves Dataverse
	// API quota). In-memory — resets on restart; the content-equality guard
	// in UpsertFlow is the persistent safety net.
	lastModified map[string]time.Time

	loop lifecycle.TickerLoop
}

// sweepTimeout bounds one ingest pass so a stalled environment (slow/queued API)
// can't block the next tick or the app's shutdown.
const sweepTimeout = 10 * time.Minute

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

	if i.lastModified == nil {
		i.lastModified = make(map[string]time.Time)
	}

	for _, f := range flows {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		// Incremental sync: skip flows whose Modified timestamp hasn't changed
		// since the last sweep. Saves a GetFlowDefinition API call per
		// unchanged flow (the bulk of a steady-state sweep). The first sweep
		// (empty map) fetches all; subsequent sweeps skip unchanged ones.
		if prev, ok := i.lastModified[f.ID]; ok && !f.Modified.IsZero() && f.Modified.Equal(prev) {
			res.Skipped++
			continue
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
		i.lastModified[f.ID] = f.Modified
		res.Ingested++
	}

	logger.Info("padcloud ingest complete",
		"listed", len(flows), "ingested", res.Ingested, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

// Start launches the periodic ingest loop: an immediate sweep, then one per
// interval. A zero/negative interval leaves the ingester disabled (no-op), so
// the wiring can always be present and config gates it. Start is idempotent.
func (i *Ingester) Start(interval time.Duration) {
	if interval <= 0 {
		return
	}
	// Derive from the loop's root ctx (both for the immediate sweep and every
	// ticked one) so Stop cancels a sweep in flight at the next flow boundary
	// instead of letting it run to completion against a shutting-down app —
	// the sweep itself respects ctx.Err() via Ingest's per-flow check.
	i.loop.Start(interval, true, func(ctx context.Context) {
		i.runSweep(ctx)
	}, func(r any) {
		logger.Error("padcloud ingest loop panicked", "err", r)
	})
}

// Stop cancels any in-flight sweep and ends the loop. Idempotent and safe to
// call on an ingester that was never started.
func (i *Ingester) Stop() {
	i.loop.Stop()
}

// runSweep runs one bounded Ingest pass and logs the outcome (the periodic loop
// ignores the error — a single failed sweep shouldn't stop the connector).
func (i *Ingester) runSweep(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("padcloud ingest sweep panicked", "err", r)
		}
	}()
	sweepCtx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	res, err := i.Ingest(sweepCtx)
	if err != nil {
		logger.Warn("padcloud ingest sweep failed", "error", err, "ingested", res.Ingested, "failed", res.Failed)
		return
	}
	for _, e := range res.Errors {
		logger.Warn("padcloud ingest flow error", "error", e)
	}
	// H20: surface loop liveness so ops can alert when the ingester hangs.
	metrics.RecordBackgroundLoopTick("padcloud_ingest")
}
