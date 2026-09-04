package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Governance alerts (desktop / single-user filesystem mode).
//
// The continuous-governance scanner is cloud-only, so in local mode this store
// is exercised mainly by the contract suite. Alerts live in a single JSON file
// (governance/alerts.json) holding an array keyed by ID; the read-modify-write
// is serialized by govAlertMu so concurrent records can't clobber each other.
// Writes are atomic (temp + rename via atomicWrite).

func (lsb *LocalStorageBackend) govAlertsPath() string {
	return filepath.Join(lsb.dataDir, "governance", "alerts.json")
}

// readGovAlerts loads the alerts file, returning an empty slice (never nil)
// when no file exists yet. Callers must hold govAlertMu.
func (lsb *LocalStorageBackend) readGovAlerts() ([]*interfaces.GovernanceAlert, error) {
	path := lsb.govAlertsPath()
	data, err := os.ReadFile(path) // #nosec G304 -- path is dataDir + a fixed relative path (no caller-controlled segment)
	if err != nil {
		if os.IsNotExist(err) {
			return []*interfaces.GovernanceAlert{}, nil
		}
		return nil, fmt.Errorf("read governance alerts: %w", err)
	}
	var out []*interfaces.GovernanceAlert
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal governance alerts: %w", err)
	}
	if out == nil {
		out = []*interfaces.GovernanceAlert{}
	}
	return out, nil
}

// writeGovAlerts atomically persists the alert list. Callers must hold govAlertMu.
func (lsb *LocalStorageBackend) writeGovAlerts(alerts []*interfaces.GovernanceAlert) error {
	path := lsb.govAlertsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create governance dir: %w", err)
	}
	data, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal governance alerts: %w", err)
	}
	return atomicWrite(path, data, 0o644)
}

func (lsb *LocalStorageBackend) RecordGovernanceAlert(ctx context.Context, a *interfaces.GovernanceAlert) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("governance alert requires an id")
	}
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return err
	}
	// Idempotent: a duplicate ID is a no-op.
	for _, ex := range alerts {
		if ex.ID == a.ID {
			return nil
		}
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	cp := *a
	alerts = append(alerts, &cp)
	return lsb.writeGovAlerts(alerts)
}

func (lsb *LocalStorageBackend) ListGovernanceAlerts(ctx context.Context, filter interfaces.GovernanceAlertFilter) ([]*interfaces.GovernanceAlert, error) {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return nil, err
	}
	// Newest-first, tie-broken by ID — mirrors the postgres backend's
	// `ORDER BY created_at DESC, id ASC`. The tiebreaker is load-bearing: this
	// list is offset-paginated, sort.Slice is not stable, and one scanner tick
	// records several alerts stamped from the same time.Now(), so without it
	// paging dropped and repeated alerts.
	sort.Slice(alerts, func(i, j int) bool {
		if !alerts[i].CreatedAt.Equal(alerts[j].CreatedAt) {
			return alerts[i].CreatedAt.After(alerts[j].CreatedAt)
		}
		return alerts[i].ID < alerts[j].ID
	})

	out := make([]*interfaces.GovernanceAlert, 0, len(alerts))
	for _, a := range alerts {
		if !filter.IncludeDismissed && a.DismissedAt != nil {
			continue
		}
		// Targeted alerts are personal: visible only to their target.
		if a.TargetUser != "" && a.TargetUser != filter.UserID {
			continue
		}
		out = append(out, a)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if filter.Offset >= len(out) {
		return out[:0], nil
	}
	out = out[filter.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (lsb *LocalStorageBackend) UnreadGovernanceAlertCount(ctx context.Context) (int, error) {
	return lsb.UnreadGovernanceAlertCountFor(ctx, "")
}

func (lsb *LocalStorageBackend) UnreadGovernanceAlertCountFor(ctx context.Context, userID string) (int, error) {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range alerts {
		if a.ReadAt == nil && a.DismissedAt == nil && (a.TargetUser == "" || a.TargetUser == userID) {
			n++
		}
	}
	return n, nil
}

func (lsb *LocalStorageBackend) MarkGovernanceAlertRead(ctx context.Context, _, alertID string) error {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	changed := false
	for _, a := range alerts {
		if a.ID == alertID && a.ReadAt == nil {
			a.ReadAt = &now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return lsb.writeGovAlerts(alerts)
}

func (lsb *LocalStorageBackend) MarkAllGovernanceAlertsRead(ctx context.Context, _ string) error {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	changed := false
	for _, a := range alerts {
		if a.ReadAt == nil && a.DismissedAt == nil {
			a.ReadAt = &now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return lsb.writeGovAlerts(alerts)
}

func (lsb *LocalStorageBackend) DismissGovernanceAlert(ctx context.Context, _, alertID string) error {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	changed := false
	for _, a := range alerts {
		if a.ID == alertID && a.DismissedAt == nil {
			a.DismissedAt = &now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return lsb.writeGovAlerts(alerts)
}

func (lsb *LocalStorageBackend) ClearGovernanceAlerts(ctx context.Context, _ string) error {
	lsb.govAlertMu.Lock()
	defer lsb.govAlertMu.Unlock()
	alerts, err := lsb.readGovAlerts()
	if err != nil {
		return err
	}
	kept := alerts[:0]
	for _, a := range alerts {
		if a.DismissedAt == nil {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(alerts) {
		return nil // nothing dismissed → no write
	}
	return lsb.writeGovAlerts(kept)
}
