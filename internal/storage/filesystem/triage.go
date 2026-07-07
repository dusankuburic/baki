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

// Finding triage & baselines (desktop / single-user filesystem mode).
//
// Triage state lives in one JSON file per flow under triage/<flowID>.json (a
// map keyed by finding key), and baselines under baselines/<flowID>.json. The
// read-modify-write of the triage map is serialized by triageMu so concurrent
// status updates can't clobber each other; writes are atomic (temp + rename).

// triagePath and baselinePath build the on-disk path for a flow's triage /
// baseline file, rejecting any flowID that could traverse outside the data dir
// (defense-in-depth parity with the service-layer convFilePath; flowIDs are
// server-generated UUIDs, but the path builder should not trust that alone).
func (lsb *LocalStorageBackend) triagePath(flowID string) (string, error) {
	if !safePathSegment(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(lsb.dataDir, "triage", flowID+".json"), nil
}

func (lsb *LocalStorageBackend) baselinePath(flowID string) (string, error) {
	if !safePathSegment(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(lsb.dataDir, "baselines", flowID+".json"), nil
}

// readStatuses loads the per-flow finding-status map, returning an empty map
// (never nil) when the flow has no triage file yet. Callers must hold triageMu.
func (lsb *LocalStorageBackend) readStatuses(flowID string) (map[string]*interfaces.FindingStatus, error) {
	path, err := lsb.triagePath(flowID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is dataDir + a flowID validated by triagePath (safePathSegment rejects traversal)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*interfaces.FindingStatus{}, nil
		}
		return nil, fmt.Errorf("read triage file: %w", err)
	}
	var m map[string]*interfaces.FindingStatus
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal triage: %w", err)
	}
	if m == nil {
		m = map[string]*interfaces.FindingStatus{}
	}
	return m, nil
}

// writeStatuses persists the per-flow finding-status map. Callers must hold triageMu.
func (lsb *LocalStorageBackend) writeStatuses(flowID string, m map[string]*interfaces.FindingStatus) error {
	path, err := lsb.triagePath(flowID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("create triage dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

func (lsb *LocalStorageBackend) SetFindingStatus(ctx context.Context, st *interfaces.FindingStatus) error {
	if st == nil || st.FlowID == "" || st.FindingKey == "" {
		return fmt.Errorf("finding status requires flowId and findingKey")
	}
	lsb.triageMu.Lock()
	defer lsb.triageMu.Unlock()

	m, err := lsb.readStatuses(st.FlowID)
	if err != nil {
		return err
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	cp := *st
	m[st.FindingKey] = &cp
	return lsb.writeStatuses(st.FlowID, m)
}

func (lsb *LocalStorageBackend) ListFindingStatuses(ctx context.Context, flowID string) ([]*interfaces.FindingStatus, error) {
	lsb.triageMu.Lock()
	defer lsb.triageMu.Unlock()

	m, err := lsb.readStatuses(flowID)
	if err != nil {
		return nil, err
	}
	out := make([]*interfaces.FindingStatus, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingKey < out[j].FindingKey })
	return out, nil
}

func (lsb *LocalStorageBackend) DeleteFindingStatus(ctx context.Context, flowID, findingKey string) error {
	lsb.triageMu.Lock()
	defer lsb.triageMu.Unlock()

	m, err := lsb.readStatuses(flowID)
	if err != nil {
		return err
	}
	if _, ok := m[findingKey]; !ok {
		return nil // idempotent
	}
	delete(m, findingKey)
	return lsb.writeStatuses(flowID, m)
}

func (lsb *LocalStorageBackend) GetFlowBaseline(ctx context.Context, flowID string) (*interfaces.FlowBaseline, error) {
	path, err := lsb.baselinePath(flowID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is dataDir + a flowID validated by baselinePath (safePathSegment rejects traversal)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read baseline: %w", err)
	}
	var bl interfaces.FlowBaseline
	if err := json.Unmarshal(data, &bl); err != nil {
		return nil, fmt.Errorf("unmarshal baseline: %w", err)
	}
	if bl.Keys == nil {
		bl.Keys = []string{}
	}
	return &bl, nil
}

func (lsb *LocalStorageBackend) SetFlowBaseline(ctx context.Context, bl *interfaces.FlowBaseline) error {
	if bl == nil || bl.FlowID == "" {
		return fmt.Errorf("baseline requires flowId")
	}
	path, err := lsb.baselinePath(bl.FlowID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("create baseline dir: %w", err)
	}
	if bl.CreatedAt.IsZero() {
		bl.CreatedAt = time.Now()
	}
	if bl.Keys == nil {
		bl.Keys = []string{}
	}
	data, err := json.MarshalIndent(bl, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

func (lsb *LocalStorageBackend) ClearFlowBaseline(ctx context.Context, flowID string) error {
	path, err := lsb.baselinePath(flowID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove baseline: %w", err)
	}
	return nil
}
