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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// errSkipped is returned by migrateOneFlow when the item is already present in dst.
var errSkipped = errors.New("already migrated")

// Result summarises the outcome of a migration run.
type Result struct {
	FlowsMigrated         int
	FlowsSkipped          int
	FlowsFailed           int
	ConversationsMigrated int
	ConversationsSkipped  int
	ConversationsFailed   int
	SettingsMoved         bool
	Errors                []MigrationError
	Duration              time.Duration
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
	// perFlowTimeout bounds a single flow's load+save so one pathological flow
	// (huge content, slow dst write) can't stall the whole run or the app's
	// shutdown. Mirrors the scanner's perFlowScanTimeout.
	perFlowTimeout time.Duration
	// convDir is the local-mode conversations directory (configDir/conversations).
	// When non-empty, conversations are migrated alongside flows; empty disables.
	convDir   string
	validator *Validator
}

// defaultPerFlowMigrationTimeout bounds each individual flow copy.
const defaultPerFlowMigrationTimeout = 60 * time.Second

// New creates a Migrator with the given source and destination.
func New(src, dst interfaces.StorageBackend) *Migrator {
	return &Migrator{
		src:            src,
		dst:            dst,
		batchSize:      50,
		perFlowTimeout: defaultPerFlowMigrationTimeout,
		validator:      NewValidator(),
	}
}

// WithBatchSize overrides the default batch size (50).
func (m *Migrator) WithBatchSize(n int) *Migrator {
	if n > 0 {
		m.batchSize = n
	}
	return m
}

// WithPerFlowTimeout overrides the per-flow timeout (default 60s).
func (m *Migrator) WithPerFlowTimeout(d time.Duration) *Migrator {
	if d > 0 {
		m.perFlowTimeout = d
	}
	return m
}

// WithConversationsDir enables conversation migration from the given local-mode
// conversations directory (configDir/conversations). An empty dir disables it.
func (m *Migrator) WithConversationsDir(dir string) *Migrator {
	m.convDir = dir
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

	// Migrate conversations (best-effort; non-fatal)
	if err := m.migrateConversations(ctx, &res); err != nil {
		logger.Warn("migration: conversations warning", "error", err)
	}

	res.Duration = time.Since(start)
	return res, nil
}

func (m *Migrator) migrateFlows(ctx context.Context, res *Result) error {
	offset := 0
	for {
		batch, err := m.src.ListFlows(ctx, interfaces.FlowFilter{
			AllFlows: true, // operational enumeration: copy every flow, unscoped
			Limit:    m.batchSize,
			Offset:   offset,
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
	// Bound a single flow's load+save so one bad flow can't stall the run.
	flowCtx, cancel := context.WithTimeout(ctx, m.perFlowTimeout)
	defer cancel()

	// Reload from source to get the full content (ListFlows may omit body)
	full, err := m.src.LoadFlow(flowCtx, flow.ID)
	if err != nil {
		return fmt.Errorf("load from source: %w", err)
	}

	if errs := m.validator.ValidateFlow(full); len(errs) > 0 {
		return fmt.Errorf("validation: %s", errs[0])
	}

	// Check if already present in destination to support idempotent reruns
	if _, err := m.dst.LoadFlow(flowCtx, full.ID); err == nil {
		return errSkipped
	}

	return m.dst.SaveFlow(flowCtx, full)
}

func (m *Migrator) migrateSettings(ctx context.Context) error {
	settings, err := m.src.LoadSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings from source: %w", err)
	}
	if settings == nil {
		return errors.New("no settings in source")
	}
	// H2: skip-if-present mirrors migrateOneFlow's idempotency. Without this
	// check, a re-run after an admin tunes cloud-mode settings silently rolls
	// them back to the (stale) local-mode values. We treat dst as "present"
	// when it has any user-visible content — analysis rule overrides or
	// recent files — so an empty fresh dst still gets seeded.
	if existing, err := m.dst.LoadSettings(ctx); err == nil && existing != nil {
		if len(existing.Analysis.Rules) > 0 || len(existing.RecentFiles) > 0 {
			logger.Info("migration: dst already has settings — skipping (clear dst to force overwrite)")
			return nil
		}
	}
	return m.dst.SaveSettings(ctx, settings)
}

// migrateConversations walks the local-mode conversations directory and copies
// each conversation file to the destination backend. Conversations are stored
// per-provider per-flow as JSON files (see ChatService.convFilePath); the
// migrator reads them directly from disk because they are not accessible via
// the StorageBackend interface (there is no ListConversations). Best-effort:
// per-file failures are recorded and never abort the run.
func (m *Migrator) migrateConversations(ctx context.Context, res *Result) error {
	if m.convDir == "" {
		return nil
	}
	info, err := os.Stat(m.convDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no conversations directory — nothing to migrate
		}
		return fmt.Errorf("stat conversations dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("conversations path is not a directory: %s", m.convDir)
	}

	walkErr := filepath.WalkDir(m.convDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if migrateErr := m.migrateOneConversation(ctx, path); migrateErr != nil {
			if errors.Is(migrateErr, errSkipped) {
				res.ConversationsSkipped++
				return nil // continue walking
			}
			res.ConversationsFailed++
			res.Errors = append(res.Errors, MigrationError{
				FlowID:  filepath.Base(path),
				Message: "conversation: " + migrateErr.Error(),
			})
			logger.Warn("migration: conversation failed", "path", path, "error", migrateErr)
			return nil // continue walking
		}
		res.ConversationsMigrated++
		return nil
	})
	if walkErr != nil {
		// Distinguish "walk was cancelled mid-sweep" (operator asked us to
		// stop) from a genuine filesystem error. Previously the cancellation
		// was silently swallowed (returned nil), hiding partial runs.
		if errors.Is(walkErr, ctx.Err()) {
			logger.Warn("migration: conversations walk cancelled mid-sweep", "migrated", res.ConversationsMigrated, "failed", res.ConversationsFailed)
			return ctx.Err()
		}
		return fmt.Errorf("walk conversations dir: %w", walkErr)
	}
	return nil
}

// migrateOneConversation reads, converts, and saves a single conversation file.
// H3: skip-if-present — if dst already has messages for (flowID, scope) the
// conversation is left untouched. Without this guard, re-running migration
// clobbers any conversations the user has continued in cloud mode since the
// first migration. Mirrors migrateOneFlow's idempotency contract.
func (m *Migrator) migrateOneConversation(ctx context.Context, path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path from WalkDir over convDir, not user input
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	flowID := conv.FlowKey
	if flowID == "" {
		flowID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	scope := conv.Scope
	if scope == "" {
		scope = filepath.Base(filepath.Dir(path))
	}

	// Skip if dst already has messages for this (flowID, scope) — re-runs must
	// not roll back post-migration chat activity.
	if existing, err := m.dst.LoadConversation(ctx, flowID, scope); err == nil && len(existing) > 0 {
		logger.Info("migration: dst already has conversation — skipping", "flowID", flowID, "scope", scope)
		return errSkipped
	}

	msgs := make([]interfaces.ChatMessage, len(conv.Messages))
	for i, msg := range conv.Messages {
		ts := ""
		if !msg.Timestamp.IsZero() {
			ts = msg.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		msgs[i] = interfaces.ChatMessage{
			ID:               msg.ID,
			Role:             msg.Role,
			Content:          msg.Content,
			Timestamp:        ts,
			ContextBlockID:   msg.ContextBlockID,
			ContextSubflowID: msg.ContextSubflowID,
			TokensIn:         msg.TokensIn,
			TokensOut:        msg.TokensOut,
			Provider:         msg.Provider,
			Model:            msg.Model,
			FinishReason:     msg.FinishReason,
		}
	}

	return m.dst.SaveConversation(ctx, flowID, scope, msgs)
}
