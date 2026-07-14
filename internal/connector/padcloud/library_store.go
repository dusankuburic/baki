package padcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// LibraryStore writes ingested flows into the shared flow library. Each flow is
// keyed by a deterministic UUID derived from the Power Platform source id, so a
// re-ingest (the periodic pull) upserts the same library row in place rather
// than duplicating it on every cycle.
type LibraryStore struct {
	backend storageif.StorageBackend
	ownerID string // service/org account that owns ingested flows (authz scope)
	orgID   string
	// ns is a fixed UUID v5 namespace so sourceID → flowID is stable across
	// re-ingests and across replicas.
	ns uuid.UUID
}

// NewLibraryStore builds a Store backed by the shared StorageBackend. ownerID
// and orgID scope the resulting flows for authorization; an operator points
// these at a service account / org so ingested flows are reachable.
func NewLibraryStore(backend storageif.StorageBackend, ownerID, orgID string) *LibraryStore {
	return &LibraryStore{
		backend: backend,
		ownerID: ownerID,
		orgID:   orgID,
		ns:      uuid.NewSHA1(uuid.NameSpaceURL, []byte("padcloud.baki/flow-id")),
	}
}

// UpsertFlow implements Store. It marshals the converted FlowDocument into the
// storage-layer FlowDocument.Content and writes it with optimistic-concurrency
// (loading the current version first so a re-ingest updates rather than 409s).
func (s *LibraryStore) UpsertFlow(ctx context.Context, doc *models.FlowDocument, sourceID string) error {
	content, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal flow: %w", err)
	}
	flowID := uuid.NewSHA1(s.ns, []byte(sourceID)).String()

	// OCC pre-check: read the existing flow so the upsert matches its version.
	// A missing row (first ingest) starts at version 0. Also compare content —
	// if the re-ingested flow is byte-identical to the stored one, skip the
	// write entirely so periodic sweeps don't bloat version history with
	// no-op snapshots.
	version := 0
	if existing, err := s.backend.LoadFlow(ctx, flowID); err == nil && existing != nil {
		version = existing.Version
		if bytes.Equal(existing.Content, content) {
			return nil // identical content — no write, no version bump
		}
	} else if err != nil && !errors.Is(err, storageif.ErrNotFound) {
		return fmt.Errorf("load flow: %w", err)
	}

	libDoc := &storageif.FlowDocument{
		ID:             flowID,
		Name:           doc.Name,
		Content:        content,
		OwnerID:        s.ownerID,
		OrganizationID: s.orgID,
		Version:        version,
		Metadata: storageif.FlowMetadata{
			BlockCount:   doc.Metadata.BlockCount,
			SubflowCount: doc.Metadata.SubflowCount,
			MaxDepth:     doc.Metadata.MaxDepth,
		},
	}
	return s.backend.SaveFlow(ctx, libDoc)
}
