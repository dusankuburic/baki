package interfaces

import (
	"context"
	"encoding/json"
	"time"
)

// FlowVersionStore covers flow version history (snapshots taken on save).
type FlowVersionStore interface {
	// SaveFlowVersion assigns v.Version atomically (caller-set value overwritten); read it back after a successful save.
	SaveFlowVersion(ctx context.Context, v *FlowVersion) error
	ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*FlowVersion, error)
	LoadFlowVersion(ctx context.Context, flowID string, version int) (*FlowVersion, error)
}

// FlowVersion stores a flow snapshot; rendered through an API view — the nested FlowMetadata carries SQL-dependent JSON keys.
type FlowVersion struct {
	ID        string
	FlowID    string
	Version   int
	Comment   string
	Content   json.RawMessage
	Metadata  FlowMetadata
	CreatedBy string
	CreatedAt time.Time
}
