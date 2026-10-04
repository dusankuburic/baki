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

// DocumentProvider abstracts the difference between single-user (local) and multi-user (cloud) flow resolution.
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

// The resolved-document cache is bounded by the heap it pins, not by entry count: docs differ in size by four
// orders of magnitude, and the old 32-ENTRY bound let a handful of multi-MiB flows pin over a gigabyte in a
// 1 GiB container (a GC spiral, then the OOM killer). The goal is unchanged — make the hot flows free, not
// cache the whole library.
const (
	// cloudDocCacheBudget is the heap all cached documents may pin together (FlowDocumentCost each).
	cloudDocCacheBudget int64 = 160 << 20
	// cloudDocCacheMaxEntry is the largest document the cache keeps: above it a document is resolved per request
	// and never cached — one such document would otherwise evict everything else for a single flow's benefit.
	cloudDocCacheMaxEntry int64 = 48 << 20
)

// Cost model for a resolved FlowDocument (memory audit 2026-10-01; synthetic_10k: 912 B/block, 7.9× its
// source): the parsed tree costs ~1 KiB per block, so a 10 MiB source (112k blocks) is ~111 MiB, plus the
// raw Source carried for cloud-mode fixes.
const (
	docCostPerBlock int64 = 1024
	docCostBase     int64 = 1024
)

// FlowDocumentCost estimates the heap doc pins, for byte-budgeted caches.
func FlowDocumentCost(doc *models.FlowDocument) int64 {
	if doc == nil {
		return 0
	}
	blocks := len(doc.BlocksByID)
	if doc.Metadata.BlockCount > blocks {
		blocks = doc.Metadata.BlockCount
	}
	return docCostBase + int64(blocks)*docCostPerBlock + int64(len(doc.Source))
}

// cloudDocEntry is a resolved document pinned to the storage version it was loaded at: any write (same process,
// another replica, the ingester) bumps the OCC version, so the next header check reloads.
type cloudDocEntry struct {
	version int
	doc     *models.FlowDocument
}

// CloudDocumentProvider loads flows from a shared storage backend, caching resolved documents keyed by
// (flowID, version). A cold resolve costs a DB row + a blob round-trip + a JSON unmarshal + an O(blocks) index
// rebuild; the cache replaces all but a cheap indexed header query. Correctness: ResolveDoc re-reads the header
// on EVERY call, so a cached doc is served only while the stored version is unchanged — no TTL, no stale window,
// safe across replicas; concurrent cold loads share one load via singleflight.
// Contract: returned docs are SHARED READ-ONLY (mutating paths build new docs). The cache is byte-weighted; a
// document above cloudDocCacheMaxEntry is served but never cached.
type CloudDocumentProvider struct {
	storage storageif.StorageBackend
	cache   *cache.SizedLRU
	sf      singleflight.Group
}

func NewCloudDocumentProvider(storage storageif.StorageBackend) *CloudDocumentProvider {
	return newCloudDocumentProviderWithBudget(storage, cloudDocCacheBudget, cloudDocCacheMaxEntry)
}

// newCloudDocumentProviderWithBudget is NewCloudDocumentProvider with the cache bounds as parameters for tests.
func newCloudDocumentProviderWithBudget(storage storageif.StorageBackend, budget, maxEntry int64) *CloudDocumentProvider {
	c := cache.NewSizedLRU(budget, cache.WithMaxEntryCost(maxEntry))
	return &CloudDocumentProvider{storage: storage, cache: c}
}

func (p *CloudDocumentProvider) ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error) {
	if p.storage == nil {
		return nil, fmt.Errorf("cloud storage not configured")
	}
	// Cheap indexed header read (no blob) — both the existence and cache-validity checks.
	header, err := p.storage.LoadFlowHeader(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.ResolveDocWithHeader(ctx, id, header)
}

// ResolveDocWithHeader is ResolveDoc for a caller that has JUST read the flow's header (GetAuthorized's authz
// check): the cache-validity check reuses it instead of reading again. The header must be current — its version
// decides whether the cached document may be served.
func (p *CloudDocumentProvider) ResolveDocWithHeader(ctx context.Context, id string, header *storageif.FlowDocument) (*models.FlowDocument, error) {
	if p.storage == nil {
		return nil, fmt.Errorf("cloud storage not configured")
	}
	doc, _, err := p.resolve(ctx, id, header, true)
	return doc, err
}

// ResolveDocUncached is ResolveDoc for a one-off read that must not displace the hot documents (the library
// search's candidate scan). A current cached document is still served (and reported as cached); a load is not
// cached, and dies with the request.
func (p *CloudDocumentProvider) ResolveDocUncached(ctx context.Context, id string) (doc *models.FlowDocument, cached bool, err error) {
	if p.storage == nil {
		return nil, false, fmt.Errorf("cloud storage not configured")
	}
	header, err := p.storage.LoadFlowHeader(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return p.resolve(ctx, id, header, false)
}

// resolve serves the cached document when its version is the header's, else loads it (one load per flow at a
// time via singleflight) and caches it if store is set and it fits. Two callers sharing one flight get the
// leader's caching decision — at worst a search-scan load leaves a concurrent resolve uncached for one request.
func (p *CloudDocumentProvider) resolve(ctx context.Context, id string, header *storageif.FlowDocument, store bool) (*models.FlowDocument, bool, error) {
	if v, ok := p.cache.Get(ctx, id); ok {
		if entry, ok := v.(*cloudDocEntry); ok && entry.version == header.Version {
			return entry.doc, true, nil
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
		if store {
			p.cache.SetWithCost(ctx, id, &cloudDocEntry{version: libDoc.Version, doc: doc}, FlowDocumentCost(doc), 0)
		}
		return doc, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.(*models.FlowDocument), false, nil
}

// resolveStoredFlow unmarshals stored flow content and applies the cloud identity overrides.
func resolveStoredFlow(id string, libDoc *storageif.FlowDocument) (*models.FlowDocument, error) {
	var doc models.FlowDocument
	if err := json.Unmarshal(libDoc.Content, &doc); err != nil {
		return nil, fmt.Errorf("invalid flow content: %w", err)
	}
	// The storage flow ID is authoritative — cloud-mode operations key on the same ID the storage layer uses.
	doc.ID = id
	doc.OwnerID = libDoc.OwnerID
	doc.OrganizationID = libDoc.OrganizationID
	// Raw PAD text for cloud-mode apply-fix/preview-fix; the column wins over the copy in Content.
	doc.Source = libDoc.Source
	// A stored flow must never carry a FilePath: every desktop file path (GetSource, SaveSource, PatchFlow,
	// snapshots) selects itself with doc.FilePath != "", and a stored one turned POST /api/library into an
	// arbitrary server file read and write. Cloud flows patch doc.Source.
	doc.FilePath = ""
	doc.RebuildIndexes()
	return &doc, nil
}

// Invalidate drops the cached document for a flow — not required for correctness (the version check self-heals),
// but immediate for a same-process delete.
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
