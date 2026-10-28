// Package migration upgrades a LEGACY filesystem-backed server (a deployment
// that ran with the filesystem storage backend and a PAD_DATA_DIR) to the
// PostgreSQL StorageBackend. It copies flows, settings and chat conversations
// only; Result.NotMigrated names what stays behind. It is not a desktop-app
// export: the desktop app keeps no store under PAD_DATA_DIR. The source is read
// through LegacySource, which never writes to the legacy directory.
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
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
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

// Step is one kind of data a legacy data directory can hold. Run copies it into
// the destination and adds its tally to the Result; a nil Run declares the kind
// as deliberately left behind, and Result.NotMigrated names it. A new kind of
// legacy data is one entry in steps.
type Step struct {
	Name string
	// Fatal aborts the run when Run fails: without the flow walk nothing else
	// is worth copying. Any other step's failure is logged and the run goes on.
	Fatal bool
	Run   func(m *Migrator, ctx context.Context, res *Result) error
}

// steps runs in order: flows first, then settings, then conversations.
var steps = []Step{
	{Name: "flows", Fatal: true, Run: (*Migrator).migrateFlows},
	{Name: "settings", Run: (*Migrator).migrateSettings},
	{Name: "conversations", Run: (*Migrator).migrateConversations},
	{Name: "users and sessions"},
	{Name: "organisations and memberships"},
	{Name: "API tokens"},
	{Name: "finding triage and comments"},
	{Name: "flow versions"},
	{Name: "share links"},
	{Name: "governance alerts"},
	{Name: "audit events"},
}

// Steps returns the declared steps in run order, the left-behind kinds included.
func Steps() []Step { return slices.Clone(steps) }

// Migrator copies a legacy data directory into a destination StorageBackend in
// batches, with basic validation before each item.
type Migrator struct {
	src       LegacySource
	dst       interfaces.StorageBackend
	batchSize int
	// perFlowTimeout bounds a single flow's load+save so one pathological flow
	// (huge content, slow dst write) can't stall the whole run or the app's
	// shutdown; mirrors the scanner's perFlowScanTimeout.
	perFlowTimeout time.Duration
	validator      *Validator
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
func New(src LegacySource, dst interfaces.StorageBackend) *Migrator {
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

// WithDefaultOwner sets the account that adopts flows whose recorded owner does
// not exist in the destination (users are not migrated, so that is the common
// case). Empty leaves owners untouched.
func (m *Migrator) WithDefaultOwner(userID string) *Migrator {
	m.defaultOwner = userID
	return m
}

// Migrate runs the steps in order. It returns a Result even on partial failure.
func (m *Migrator) Migrate(ctx context.Context) (Result, error) {
	start := time.Now()
	res := Result{NotMigrated: []string{}}
	for _, s := range steps {
		if s.Run == nil {
			res.NotMigrated = append(res.NotMigrated, s.Name)
		}
	}
	m.adopted = 0

	if err := m.dst.Ping(ctx); err != nil {
		return res, fmt.Errorf("destination unreachable: %w", err)
	}

	for _, s := range steps {
		if s.Run == nil {
			continue
		}
		if err := s.Run(m, ctx, &res); err != nil {
			if s.Fatal {
				// Steps accumulate per-item errors into res.Errors; a
				// returned error is an infrastructure failure.
				res.FlowsAdopted = m.adopted
				res.Duration = time.Since(start)
				return res, fmt.Errorf("%s migration aborted: %w", s.Name, err)
			}
			logger.Warn("migration: "+s.Name+" warning", "error", err)
		}
	}

	res.FlowsAdopted = m.adopted
	res.Duration = time.Since(start)
	return res, nil
}

func (m *Migrator) migrateFlows(ctx context.Context, res *Result) error {
	after := ""
	for {
		// Page by the immutable primary key: any ordering on a mutable column
		// lets rows shift between pages, silently skipping flows while the run
		// still reports success and no errors (data loss presented as a clean
		// migration). The listing carries IDs only; migrateOneFlow loads the
		// flows it actually copies.
		ids, err := m.src.ListFlowIDs(ctx, after, m.batchSize)
		if err != nil {
			return fmt.Errorf("list flows (after %q): %w", after, err)
		}

		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := m.migrateOneFlow(ctx, id); err != nil {
				if errors.Is(err, errSkipped) {
					res.FlowsSkipped++
					continue
				}
				res.FlowsFailed++
				res.Errors = append(res.Errors, MigrationError{
					FlowID:  id,
					Message: err.Error(),
				})
				logger.Error("migration: flow failed", "flowID", id, "error", err)
				continue
			}
			res.FlowsMigrated++
		}

		if len(ids) < m.batchSize {
			return nil // last page
		}
		last := ids[len(ids)-1]
		if last <= after {
			return fmt.Errorf("list flows (after %q): the source did not advance past %q", after, last)
		}
		after = last
	}
}

func (m *Migrator) migrateOneFlow(ctx context.Context, id string) error {
	// Bound a single flow's load+save so one bad flow can't stall the run.
	flowCtx, cancel := context.WithTimeout(ctx, m.perFlowTimeout)
	defer cancel()

	// Check if already present in destination to support idempotent reruns.
	// The header is enough (a LoadFlow fetched the row AND downloaded the
	// content blob), and it comes BEFORE the source load: a rerun used to
	// hydrate every already-migrated flow from the source only to skip it.
	if _, err := m.dst.LoadFlowHeader(flowCtx, id); err == nil {
		return errSkipped
	}

	full, err := m.src.LoadFlow(flowCtx, id)
	if err != nil {
		return fmt.Errorf("load from source: %w", err)
	}

	if errs := m.validator.ValidateFlow(full); len(errs) > 0 {
		return fmt.Errorf("validation: %s", errs[0])
	}

	adopted := m.adoptOwner(flowCtx, full)
	// ImportFlow, not SaveFlow: the copy keeps the flow's created and updated
	// times, which SaveFlow would stamp with the time of the run.
	if err := m.dst.ImportFlow(flowCtx, full); errors.Is(err, interfaces.ErrVersionConflict) {
		return errSkipped // stored since the existence check above
	} else if err != nil {
		return err
	}
	if adopted {
		m.adopted++
	}
	return nil
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

func (m *Migrator) migrateSettings(ctx context.Context, res *Result) error {
	settings, err := m.src.LoadSettings(ctx)
	if errors.Is(err, interfaces.ErrNotFound) || (err == nil && settings == nil) {
		// Nothing to move. The destination keeps its own settings instead of
		// being handed defaults the legacy server never stored.
		return errors.New("no settings in source")
	}
	if err != nil {
		return fmt.Errorf("load settings from source: %w", err)
	}
	// H2: skip-if-present mirrors migrateOneFlow's idempotency. Without this
	// check, a re-run after an admin tunes cloud-mode settings silently rolls
	// them back to the stale local-mode values. dst counts as "present" when
	// it has any user-visible content — analysis rule overrides or recent
	// files — so an empty fresh dst still gets seeded.
	if existing, err := m.dst.LoadSettings(ctx); err == nil && existing != nil {
		if len(existing.Analysis.Rules) > 0 || len(existing.RecentFiles) > 0 {
			logger.Info("migration: dst already has settings — skipping (clear dst to force overwrite)")
			res.SettingsMoved = true
			return nil
		}
	}
	if err := m.dst.SaveSettings(ctx, settings); err != nil {
		return err
	}
	res.SettingsMoved = true
	return nil
}

// migrateConversations copies every conversation the source lists.
// Best-effort: per-conversation failures are recorded and never abort the run.
func (m *Migrator) migrateConversations(ctx context.Context, res *Result) error {
	refs, err := m.src.ListConversations(ctx)
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}
	for _, ref := range refs {
		// A cancellation (the operator asked us to stop) is reported apart
		// from a source error; it used to be swallowed, hiding partial runs.
		if err := ctx.Err(); err != nil {
			logger.Warn("migration: conversations walk cancelled mid-sweep", "migrated", res.ConversationsMigrated, "failed", res.ConversationsFailed)
			return err
		}
		if err := m.migrateOneConversation(ctx, ref); err != nil {
			if errors.Is(err, errSkipped) {
				res.ConversationsSkipped++
				continue
			}
			res.ConversationsFailed++
			res.Errors = append(res.Errors, MigrationError{
				FlowID:  filepath.Base(ref.Path),
				Message: "conversation: " + err.Error(),
			})
			logger.Warn("migration: conversation failed", "path", ref.Path, "error", err)
			continue
		}
		res.ConversationsMigrated++
	}
	return nil
}

// migrateOneConversation reads and saves a single conversation.
// H3: skip-if-present — if dst already has messages for (flowID, scope) the
// conversation is left untouched, so re-running migration cannot clobber
// conversations the user has continued in cloud mode since the first run.
// Mirrors migrateOneFlow's idempotency contract.
//
// The thread is imported as the importing user's (the default owner, R-security-3): a desktop has one user, and the
// owner's thread index then lists it. The write is a create (SaveConversationIf at version 0), so a conversation
// already stored under the key — an owned one with no messages yet (a thread named before its first turn) included,
// which the message check above lets through — is never replaced: the create conflicts, and the thread is skipped.
func (m *Migrator) migrateOneConversation(ctx context.Context, ref ConversationRef) error {
	conv, err := m.src.LoadConversation(ctx, ref)
	if err != nil {
		return err
	}
	if existing, err := m.dst.LoadConversation(ctx, conv.FlowID, conv.Scope); err == nil && len(existing) > 0 {
		logger.Info("migration: dst already has conversation — skipping", "flowID", conv.FlowID, "scope", conv.Scope)
		return errSkipped
	}
	_, err = m.dst.SaveConversationIf(ctx, conv.FlowID, conv.Scope, m.defaultOwner, 0, conv.Messages)
	if errors.Is(err, interfaces.ErrConversationConflict) || errors.Is(err, interfaces.ErrConversationKeyTaken) {
		logger.Info("migration: dst already has conversation — skipping", "flowID", conv.FlowID, "scope", conv.Scope)
		return errSkipped
	}
	return err
}
