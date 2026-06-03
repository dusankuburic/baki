// Package migration handles data migration from the local filesystem backend
// to a cloud-backed (PostgreSQL) StorageBackend.
//
// # Failure model
//
// Migration is **best-effort and idempotent**, not transactional:
//
//   - Per-flow failures (validation errors, dst write errors) are collected
//     into Result.Errors. They never abort the run; the migrator continues
//     with the next flow so a single bad row doesn't strand thousands of
//     good ones.
//   - Result.FlowsMigrated / FlowsSkipped / FlowsFailed give the operator
//     a precise tally. If Errors is non-empty the operator should inspect
//     them, address the root cause, and re-run.
//   - Re-runs are safe: migrateOneFlow skips any flow already present in
//     the destination (matched by ID). A partially-completed migration is
//     completed by simply running Migrate again.
//   - A non-nil error from Migrate signals a *fatal* infrastructure
//     failure (destination unreachable, list query failed) — not per-flow
//     issues. The Result is still returned and may contain partial progress.
//
// There is intentionally no rollback. Reverting a partial cloud migration
// would require knowing which flows the operator already touched
// out-of-band and would conflict with the idempotent-rerun model.
package migration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/storage/interfaces"
)

// errSkipped is returned by migrateOneFlow when the item is already present in dst.
var errSkipped = errors.New("already migrated")

// Result summarises the outcome of a migration run.
type Result struct {
	FlowsMigrated  int
	FlowsSkipped   int
	FlowsFailed    int
	SettingsMoved  bool
	Errors         []MigrationError
	Duration       time.Duration
}

// MigrationError captures a per-item failure without stopping the run.
type MigrationError struct {
	FlowID  string
	Message string
}

// Migrator copies data from a source StorageBackend to a destination
// StorageBackend in batches, with basic validation before each item.
type Migrator struct {
	src       interfaces.StorageBackend
	dst       interfaces.StorageBackend
	batchSize int
	validator *Validator
}

// New creates a Migrator with the given source and destination.
func New(src, dst interfaces.StorageBackend) *Migrator {
	return &Migrator{
		src:       src,
		dst:       dst,
		batchSize: 50,
		validator: NewValidator(),
	}
}

// WithBatchSize overrides the default batch size (50).
func (m *Migrator) WithBatchSize(n int) *Migrator {
	if n > 0 {
		m.batchSize = n
	}
	return m
}

// Migrate runs the full migration: flows first, then settings, then
// conversations (best-effort).  It returns a Result even on partial failure.
func (m *Migrator) Migrate(ctx context.Context) (Result, error) {
	start := time.Now()
	var res Result

	if err := m.dst.Ping(ctx); err != nil {
		return res, fmt.Errorf("destination unreachable: %w", err)
	}

	// Migrate flows
	if err := m.migrateFlows(ctx, &res); err != nil {
		// migrateFlows accumulates per-item errors into res.Errors;
		// a non-nil return signals a fatal infrastructure failure.
		res.Duration = time.Since(start)
		return res, fmt.Errorf("flows migration aborted: %w", err)
	}

	// Migrate settings (non-fatal if absent)
	if err := m.migrateSettings(ctx); err != nil {
		logger.Warn("migration: settings warning", "error", err)
	} else {
		res.SettingsMoved = true
	}

	res.Duration = time.Since(start)
	return res, nil
}

func (m *Migrator) migrateFlows(ctx context.Context, res *Result) error {
	offset := 0
	for {
		batch, err := m.src.ListFlows(ctx, interfaces.FlowFilter{
			Limit:  m.batchSize,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("list flows (offset %d): %w", offset, err)
		}
		if len(batch) == 0 {
			break
		}

		for _, flow := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := m.migrateOneFlow(ctx, flow); err != nil {
				if errors.Is(err, errSkipped) {
					res.FlowsSkipped++
					continue
				}
				res.FlowsFailed++
				res.Errors = append(res.Errors, MigrationError{
					FlowID:  flow.ID,
					Message: err.Error(),
				})
				logger.Error("migration: flow failed", "flowID", flow.ID, "error", err)
				continue
			}
			res.FlowsMigrated++
		}

		offset += len(batch)
		if len(batch) < m.batchSize {
			break // last page
		}
	}
	return nil
}

func (m *Migrator) migrateOneFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	// Reload from source to get the full content (ListFlows may omit body)
	full, err := m.src.LoadFlow(ctx, flow.ID)
	if err != nil {
		return fmt.Errorf("load from source: %w", err)
	}

	if errs := m.validator.ValidateFlow(full); len(errs) > 0 {
		return fmt.Errorf("validation: %s", errs[0])
	}

	// Check if already present in destination to support idempotent reruns
	if _, err := m.dst.LoadFlow(ctx, full.ID); err == nil {
		return errSkipped
	}

	return m.dst.SaveFlow(ctx, full)
}

func (m *Migrator) migrateSettings(ctx context.Context) error {
	settings, err := m.src.LoadSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings from source: %w", err)
	}
	if settings == nil {
		return errors.New("no settings in source")
	}
	return m.dst.SaveSettings(ctx, settings)
}
