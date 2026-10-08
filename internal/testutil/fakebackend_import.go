package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// ImportFlow inserts f at version 0 with its own created and updated times (zero: now, then the created time), as
// Postgres does for the legacy migration. A stored flow is left alone and reported as ErrVersionConflict.
func (m *FakeBackend) ImportFlow(_ context.Context, f *interfaces.FlowDocument) error {
	if err := validFlowID(f.ID); err != nil {
		return err
	}
	content := f.Content
	if len(content) == 0 {
		content = json.RawMessage("{}")
	}
	created, updated := f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	if f.CreatedAt.IsZero() {
		created = time.Now().UTC()
	}
	if f.UpdatedAt.IsZero() {
		updated = created
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Flows[f.ID]; ok {
		return interfaces.ErrVersionConflict
	}
	if m.Flows == nil {
		m.Flows = make(map[string]*interfaces.FlowDocument)
	}
	m.Flows[f.ID] = &interfaces.FlowDocument{
		ID: f.ID, Name: f.Name, Description: f.Description, Content: bytes.Clone(content), Source: f.Source,
		Metadata: f.Metadata, OwnerID: f.OwnerID, OrganizationID: f.OrganizationID, CreatedAt: created, UpdatedAt: updated,
	}
	f.CreatedAt, f.UpdatedAt, f.Version = created, updated, 0
	return nil
}
