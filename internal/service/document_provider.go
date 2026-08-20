package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
	"pad-core/models"
)

// DocumentProvider abstracts the difference between single-user (local)
// and multi-user (cloud) flow resolution.
type DocumentProvider interface {
	ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error)
	SetCurrentDoc(doc *models.FlowDocument)
	CurrentDoc() *models.FlowDocument
}

// LocalDocumentProvider holds a single current flow in memory.
type LocalDocumentProvider struct {
	mu  sync.RWMutex
	doc *models.FlowDocument
}

func NewLocalDocumentProvider() *LocalDocumentProvider {
	return &LocalDocumentProvider{}
}

func (p *LocalDocumentProvider) ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.doc == nil {
		return nil, ErrUninitialized
	}
	return p.doc, nil
}

func (p *LocalDocumentProvider) SetCurrentDoc(doc *models.FlowDocument) {
	p.mu.Lock()
	p.doc = doc
	p.mu.Unlock()
}

func (p *LocalDocumentProvider) CurrentDoc() *models.FlowDocument {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.doc
}

// cloudDocCacheSize bounds the resolved-document cache. Resolved docs are the
// dominant per-request memory in cloud mode (a full FlowDocument per flow), so
// the bound is modest — the goal is to make the hot flows (the one being
// viewed/edited) free, not to cache the whole library.
const cloudDocCacheSize = 32

// cloudDocEntry is a resolved document pinned to the storage version it was
// loaded at. The version comparison is the cache's correctness mechanism: any
// write (same process, another replica, the PAD-cloud ingester) bumps the
// flow's OCC version, so the next header check sees a mismatch and reloads.
type cloudDocEntry struct {
	version int
	doc     *models.FlowDocument
}

// CloudDocumentProvider loads flows from a shared storage backend, caching the
// resolved documents keyed by (flowID, version).
//
// A cold resolve costs a DB row read + a blob round-trip + a full JSON
// unmarshal + an O(blocks) index rebuild — repeated per request on every
// flow-scoped endpoint (search-as-you-type, diff, exports, every chat turn).
// The cache replaces all but a cheap indexed header query on warm resolves.
//
// Correctness: SaveFlow bumps the flow's version on every write (OCC), and
// ResolveDoc re-reads the header on EVERY call, so a cached doc is only served
// while the stored version is unchanged — no TTL, no stale window, safe across
// replicas. Concurrent cold loads of the same flow share one load via
// singleflight.
//
// Contract: returned docs are SHARED READ-ONLY. Callers must not mutate them
// (mutating paths — apply-fix, save-source — build new docs). This matches the
// LocalDocumentProvider, which already hands the same pointer to every caller.
type CloudDocumentProvider struct {
	storage storageif.StorageBackend
	cache   cache.Cache
	sf      singleflight.Group
}

func NewCloudDocumentProvider(storage storageif.StorageBackend) *CloudDocumentProvider {
	c, _ := cache.NewLRUCache(cloudDocCacheSize) // size > 0 ⇒ error impossible
	return &CloudDocumentProvider{storage: storage, cache: c}
}

func (p *CloudDocumentProvider) ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error) {
	if p.storage == nil {
		return nil, fmt.Errorf("cloud storage not configured")
	}
	// Cheap indexed header read (no blob) — both the existence check and the
	// cache-validity check. NotFound propagates exactly as a cold LoadFlow
	// would have returned it.
	header, err := p.storage.LoadFlowHeader(ctx, id)
	if err != nil {
		return nil, err
	}
	if v, ok := p.cache.Get(ctx, id); ok {
		if entry, ok := v.(*cloudDocEntry); ok && entry.version == header.Version {
			return entry.doc, nil
		}
	}
	v, err, _ := p.sf.Do(id, func() (any, error) {
		libDoc, err := p.storage.LoadFlow(ctx, id)
		if err != nil {
			return nil, err
		}
		doc, err := resolveStoredFlow(id, libDoc)
		if err != nil {
			return nil, err
		}
		p.cache.Set(ctx, id, &cloudDocEntry{version: libDoc.Version, doc: doc}, 0)
		return doc, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*models.FlowDocument), nil
}

// resolveStoredFlow unmarshals stored flow content into a FlowDocument and
// applies the cloud identity overrides.
func resolveStoredFlow(id string, libDoc *storageif.FlowDocument) (*models.FlowDocument, error) {
	var doc models.FlowDocument
	if err := json.Unmarshal(libDoc.Content, &doc); err != nil {
		return nil, fmt.Errorf("invalid flow content: %w", err)
	}
	// The storage flow ID is authoritative — override the parser-minted UUID
	// inside Content, so cloud-mode operations (apply-fix/preview-fix/save)
	// key on the same ID the storage layer uses.
	doc.ID = id
	doc.OwnerID = libDoc.OwnerID
	doc.OrganizationID = libDoc.OrganizationID
	doc.Source = libDoc.Source // raw PAD text for cloud-mode apply-fix/preview-fix
	doc.RebuildIndexes()
	return &doc, nil
}

// Invalidate drops the cached document for a flow. Not required for
// correctness (the version check self-heals after any write) but available to
// callers that want an immediate drop, e.g. right after a same-process delete.
func (p *CloudDocumentProvider) Invalidate(flowID string) {
	p.cache.Delete(context.Background(), flowID)
}

func (p *CloudDocumentProvider) SetCurrentDoc(doc *models.FlowDocument) {
	// No-op in cloud mode
}

func (p *CloudDocumentProvider) CurrentDoc() *models.FlowDocument {
	// No concept of "current" doc across requests in cloud mode
	return nil
}
