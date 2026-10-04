package analyzer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pad-core/models"
)

type AnalysisSnapshot struct {
	Timestamp   string `json:"timestamp"`
	FlowID      string `json:"flowId"`
	Hash        string `json:"hash"`
	Errors      int    `json:"errors"`
	Warnings    int    `json:"warnings"`
	Info        int    `json:"info"`
	HealthScore int    `json:"healthScore"`
	DurationMs  int    `json:"durationMs"`
}

type HistoryStore struct {
	// mu guards only the flowLocks registry — held just long enough to hand
	// out a flow's lock, never across disk I/O (an earlier design held it
	// across Record's whole round trip, serializing records of DIFFERENT flows).
	mu sync.Mutex
	// flowLocks holds one mutex per flow history file: Record's
	// read-merge-write is atomic only with one writer per flow at a time.
	// Keyed by flow so distinct flows proceed in parallel; entries are never
	// removed (one pointer each).
	flowLocks  map[string]*sync.Mutex
	dir        string
	maxPerFlow int
}

func NewHistoryStore(dir string) *HistoryStore {
	return &HistoryStore{dir: dir, maxPerFlow: 100}
}

// lockFor returns the mutex serializing access to flowID's history file,
// filled lazily so a zero-value HistoryStore works without a constructor.
func (h *HistoryStore) lockFor(flowID string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flowLocks == nil {
		h.flowLocks = make(map[string]*sync.Mutex)
	}
	flowMu := h.flowLocks[flowID]
	if flowMu == nil {
		flowMu = &sync.Mutex{}
		h.flowLocks[flowID] = flowMu
	}
	return flowMu
}

func (h *HistoryStore) Record(flowID string, report *models.AnalysisReport, doc *models.FlowDocument) {
	if h.dir == "" {
		return
	}

	// Prefer the hash pinned on the report by the analysis-cache path —
	// recomputing FlowHash would walk the whole document again; non-cached
	// paths (tests, direct runAnalysisCore callers) fall back to computing it.
	contentHash := report.ContentHash
	if contentHash == "" && doc != nil {
		contentHash = FlowHash(doc)
	}
	snapshot := AnalysisSnapshot{
		Timestamp:   time.Now().Format(time.RFC3339),
		FlowID:      flowID,
		Hash:        contentHash,
		Errors:      report.Stats.Errors,
		Warnings:    report.Stats.Warnings,
		Info:        report.Stats.Info,
		HealthScore: 100,
		DurationMs:  report.DurationMs,
	}
	if report.Metrics != nil {
		snapshot.HealthScore = report.Metrics.HealthScore
	}

	// Snapshot construction stays outside the flow lock (reads only the
	// arguments); the read-merge-write below is what must be serialized per
	// flow, and lockFor gives each flow its own mutex so distinct flows never
	// contend.
	flowMu := h.lockFor(flowID)
	flowMu.Lock()
	defer flowMu.Unlock()

	if err := os.MkdirAll(h.dir, 0750); err != nil {
		return
	}

	path := filepath.Join(h.dir, flowID+".json")
	var snapshots []AnalysisSnapshot
	data, err := os.ReadFile(path) // #nosec G304 -- path = h.dir/<flowID>.json, not raw user input
	if err == nil {
		_ = json.Unmarshal(data, &snapshots) // best-effort; a corrupt file is treated as empty history
	}

	// Skip duplicate snapshots: re-analyzing unchanged content (cache hits,
	// repeated clicks) must not flood the trend history with identical points.
	if n := len(snapshots); n > 0 {
		last := snapshots[n-1]
		if last.Hash == snapshot.Hash &&
			last.Errors == snapshot.Errors &&
			last.Warnings == snapshot.Warnings &&
			last.Info == snapshot.Info {
			return
		}
	}

	snapshots = append(snapshots, snapshot)
	if len(snapshots) > h.maxPerFlow {
		snapshots = snapshots[len(snapshots)-h.maxPerFlow:]
	}

	encoded, _ := json.Marshal(snapshots)
	_ = os.WriteFile(path, encoded, 0600) // best-effort trend persistence; internal app data
}

func (h *HistoryStore) Load(flowID string) []AnalysisSnapshot {
	if h.dir == "" {
		return nil
	}

	// Same per-flow discipline as Record: a Load racing a Record of the same
	// flow observes either the old or the new snapshot list, never a file
	// mid-rewrite; loads of different flows do not contend.
	flowMu := h.lockFor(flowID)
	flowMu.Lock()
	defer flowMu.Unlock()

	path := filepath.Join(h.dir, flowID+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- path = h.dir/<flowID>.json, not raw user input
	if err != nil {
		return nil
	}

	var snapshots []AnalysisSnapshot
	_ = json.Unmarshal(data, &snapshots) // best-effort; a corrupt file yields no history
	return snapshots
}
