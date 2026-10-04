// Package migration upgrades a LEGACY filesystem-backed server (a deployment
// that ran with the filesystem storage backend and a PAD_DATA_DIR) to the
// PostgreSQL StorageBackend. It copies flows, settings and chat conversations
// only; Result.NotMigrated names what stays behind. It is not a desktop-app
// export: the desktop app keeps no store under PAD_DATA_DIR.
//
// # Failure model
//
// Migration is best-effort and idempotent, not transactional. Per-flow
// failures (validation, dst write errors) are collected into Result.Errors and
// never abort the run, so a single bad row doesn't strand thousands of good
// ones; FlowsMigrated/FlowsSkipped/FlowsFailed give the operator a tally, and
// re-runs are safe (migrateOneFlow skips flows already present by ID). A
// non-nil error from Migrate signals a fatal infrastructure failure
// (destination unreachable, list query failed); the Result may still hold
// partial progress. There is intentionally no rollback: reverting a partial
// migration would require knowing which flows the operator already touched
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
	// FlowsAdopted counts flows whose owner did not exist in the destination
	// and were therefore assigned to the operator who ran the migration.
	FlowsAdopted int
	// NotMigrated lists the kinds of data the source directory can hold that
	// this migrator does not copy (the status used to report flow counts
	// only, so a "completed" run read as a full upgrade).
	NotMigrated []string
	Errors      []MigrationError
	Duration    time.Duration
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
	// shutdown; mirrors the scanner's perFlowScanTimeout.
	perFlowTimeout time.Duration
	// convDir is the local-mode conversations directory (configDir/conversations).
	// When non-empty, conversations are migrated alongside flows; empty disables.
	convDir   string
	validator *Validator
	// defaultOwner adopts flows whose owner does not exist in the destination
	// (users are not migrated, so that is the common case): without adoption
	// the flow is owned by nobody and invisible in every library and
	// portfolio view while the run still reports it migrated. Empty keeps the
	// source owner as-is.
	defaultOwner string
	// ownerKnown caches destination user lookups for the run.
	ownerKnown map[string]bool
	// adopted counts reassignments made during the current run.
	adopted int
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
		ownerKnown:     map[string]bool{},
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

// WithDefaultOwner sets the account that adopts flows whose recorded owner does
// not exist in the destination (users are not migrated, so that is the common
// case). Empty leaves owners untouched.
func (m *Migrator) WithDefaultOwner(userID string) *Migrator {
	m.defaultOwner = userID
	return m
}

// Migrate runs the full migration: flows first, then settings, then
// conversations (best-effort). It returns a Result even on partial failure.
// notMigrated is what a legacy filesystem data directory can hold beyond the
// flows, settings and conversations Migrate copies.
var notMigrated = []string{
	"users and sessions",
	"organisations and memberships",
	"API tokens",
	"finding triage and comments",
	"flow versions",
	"share links",
	"governance alerts",
	"audit events",
}

func (m *Migrator) Migrate(ctx context.Context) (Result, error) {
	start := time.Now()
	res := Result{NotMigrated: append([]string(nil), notMigrated...)}
	m.adopted = 0

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

	res.FlowsAdopted = m.adopted
	res.Duration = time.Since(start)
	return res, nil
}

func (m *Migrator) migrateFlows(ctx context.Context, res *Result) error {
	offset := 0
	for {
		batch, err := m.src.ListFlows(ctx, interfaces.FlowFilter{
			AllFlows: true, // operational enumeration: copy every flow, unscoped
			// Order by the immutable primary key, not the default updated_at
			// DESC: this walk pages with LIMIT/OFFSET, and any ordering on a
			// mutable column lets rows shift between pages — silently skipping
			// flows while the run still reports success and no errors (data
			// loss presented as a clean migration).
			SortBy: interfaces.FlowSortIDAsc,
			Limit:  m.batchSize,
			Offset: offset,
			// The listing only drives the walk (ids to copy); migrateOneFlow
			// reloads each flow it actually copies. Without this every page
			// hydrated every flow's content — including the ones a rerun then
			// skipped as already migrated.
			MetadataOnly: true,
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

	// Check if already present in destination to support idempotent reruns.
	// The header is enough (a LoadFlow fetched the row AND downloaded the
	// content blob), and it comes BEFORE the source load: a rerun used to
	// hydrate every already-migrated flow from the source only to skip it.
	if _, err := m.dst.LoadFlowHeader(flowCtx, flow.ID); err == nil {
		return errSkipped
	}

	// Reload from source to get the full content (the listing is metadata-only)
	full, err := m.src.LoadFlow(flowCtx, flow.ID)
	if err != nil {
		return fmt.Errorf("load from source: %w", err)
	}

	if errs := m.validator.ValidateFlow(full); len(errs) > 0 {
		return fmt.Errorf("validation: %s", errs[0])
	}

	if m.adoptOwner(flowCtx, full) {
		m.adopted++
	}
	return m.dst.SaveFlow(flowCtx, full)
}

// adoptOwner points a flow at the operator when its recorded owner is absent
// from the destination, and reports whether it did. Without this the flow is
// stored owned by an account that does not exist, which no library, portfolio
// or search query can return — a migration that reports success and shows
// nothing.
func (m *Migrator) adoptOwner(ctx context.Context, flow *interfaces.FlowDocument) bool {
	if m.defaultOwner == "" || flow.OwnerID == m.defaultOwner {
		return false
	}
	if flow.OwnerID != "" {
		known, cached := m.ownerKnown[flow.OwnerID]
		if !cached {
			u, err := m.dst.LoadUserByID(ctx, flow.OwnerID)
			known = err == nil && u != nil
			m.ownerKnown[flow.OwnerID] = known
		}
		if known {
			return false
		}
	}
	flow.OwnerID = m.defaultOwner
	return true
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
	// them back to the stale local-mode values. dst counts as "present" when
	// it has any user-visible content — analysis rule overrides or recent
	// files — so an empty fresh dst still gets seeded.
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
// migrator reads them directly from disk because the StorageBackend interface
// has no ListConversations. Best-effort: per-file failures are recorded and
// never abort the run.
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
		// stop) from a genuine filesystem error — the cancellation used to be
		// silently swallowed, hiding partial runs.
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
// conversation is left untouched, so re-running migration cannot clobber
// conversations the user has continued in cloud mode since the first run.
// Mirrors migrateOneFlow's idempotency contract.
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
			ServedModel:      msg.ServedModel,
			InputCostPerM:    msg.InputCostPerM,
			OutputCostPerM:   msg.OutputCostPerM,
			FinishReason:     msg.FinishReason,
		}
	}

	return m.dst.SaveConversation(ctx, flowID, scope, msgs)
}
