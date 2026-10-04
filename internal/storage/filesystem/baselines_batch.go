package filesystem

import (
	"context"

	"pad-analyzer/internal/storage/interfaces"
)

// GetFlowBaselinesBatch resolves many flows' baselines (interfaces.TriageStore).
// Baselines are one file per flow here, so the batch is the per-flow read in a
// loop — the method exists for the Postgres backend's single ANY($1) query;
// the governance scanner that pages with it is cloud-only. Flows without a
// baseline are absent from the map, which is never nil.
func (lsb *LocalStorageBackend) GetFlowBaselinesBatch(ctx context.Context, flowIDs []string) (map[string]*interfaces.FlowBaseline, error) {
	out := make(map[string]*interfaces.FlowBaseline, len(flowIDs))
	for _, id := range flowIDs {
		bl, err := lsb.GetFlowBaseline(ctx, id)
		if err != nil {
			return nil, err
		}
		if bl != nil {
			out[id] = bl
		}
	}
	return out, nil
}
