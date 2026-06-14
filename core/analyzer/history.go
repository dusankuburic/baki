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
	Timestamp string `json:"timestamp"`
	FlowID    string `json:"flowId"`
	Hash      string `json:"hash"`
	Errors    int    `json:"errors"`
	Warnings  int    `json:"warnings"`
	Info      int    `json:"info"`
	HealthScore int  `json:"healthScore"`
	DurationMs int   `json:"durationMs"`
}

type HistoryStore struct {
	mu       sync.Mutex
	dir      string
	maxPerFlow int
}

func NewHistoryStore(dir string) *HistoryStore {
	return &HistoryStore{dir: dir, maxPerFlow: 100}
}

func (h *HistoryStore) Record(flowID string, report *models.AnalysisReport, doc *models.FlowDocument) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.dir == "" {
		return
	}

	snapshot := AnalysisSnapshot{
		Timestamp:   time.Now().Format(time.RFC3339),
		FlowID:      flowID,
		Hash:        FlowHash(doc),
		Errors:      report.Stats.Errors,
		Warnings:    report.Stats.Warnings,
		Info:        report.Stats.Info,
		HealthScore: 100,
		DurationMs:  report.DurationMs,
	}
	if report.Metrics != nil {
		snapshot.HealthScore = report.Metrics.HealthScore
	}

	if err := os.MkdirAll(h.dir, 0755); err != nil {
		return
	}

	path := filepath.Join(h.dir, flowID+".json")
	var snapshots []AnalysisSnapshot
	data, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal(data, &snapshots)
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
	os.WriteFile(path, encoded, 0644)
}

func (h *HistoryStore) Load(flowID string) []AnalysisSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.dir == "" {
		return nil
	}

	path := filepath.Join(h.dir, flowID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var snapshots []AnalysisSnapshot
	json.Unmarshal(data, &snapshots)
	return snapshots
}
