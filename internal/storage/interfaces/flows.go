package interfaces

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrVersionConflict is returned by SaveFlow when the expected version does not match the current row version (optimistic concurrency; map to HTTP 409).
var ErrVersionConflict = errors.New("version conflict: the flow was modified by another user")

// FlowStore covers flow document CRUD/listing; depend on it rather than the full StorageBackend when that is all you need.
type FlowStore interface {
	SaveFlow(ctx context.Context, flow *FlowDocument) error
	// ImportFlow inserts a flow not stored yet at version 0 keeping its CreatedAt/UpdatedAt (zero: now, then the created time), for the legacy migration; a stored flow is left alone and reported as ErrVersionConflict.
	ImportFlow(ctx context.Context, flow *FlowDocument) error
	// UpdateFlowTags replaces a flow's tags WITHOUT a content round-trip or version bump; implementations normalize (see NormalizeFlowTags).
	UpdateFlowTags(ctx context.Context, flowID string, tags []string) error
	// SearchFlowContents returns METADATA-ONLY documents whose content mentions needle, newest first, in the filter's scope; backends without a queryable content column return ErrContentSearchUnsupported.
	SearchFlowContents(ctx context.Context, filter FlowFilter, needle string, limit int) ([]*FlowDocument, error)
	LoadFlow(ctx context.Context, id string) (*FlowDocument, error)
	// LoadFlowHeader returns a flow's metadata WITHOUT content, so authorization/existence/version checks avoid the content fetch and blob availability.
	LoadFlowHeader(ctx context.Context, id string) (*FlowDocument, error)
	ListFlows(ctx context.Context, filter FlowFilter) ([]*FlowDocument, error)
	// CountFlows returns the total matching the filter, ignoring Limit/Offset (list pagination totals).
	CountFlows(ctx context.Context, filter FlowFilter) (int, error)
	// ListFlowsWithTotal is ListFlows plus CountFlows of the same filter in one call (one statement on Postgres); a page past the end still reports the total.
	ListFlowsWithTotal(ctx context.Context, filter FlowFilter) ([]*FlowDocument, int, error)
	DeleteFlow(ctx context.Context, id string) error
}

// FlowSort enumerates the supported sort orders for ListFlows; the zero value (FlowSortUpdatedDesc) is the historical default.
type FlowSort int

const (
	FlowSortUpdatedDesc FlowSort = iota
	FlowSortUpdatedAsc
	FlowSortNameAsc
	FlowSortNameDesc
	FlowSortBlocksDesc
	// FlowSortIDAsc orders by the primary key so whole-table LIMIT/OFFSET walks (migrator, governance scanner) see each row exactly once; other sorts order by MUTABLE columns, where a mid-walk save shifts later rows.
	FlowSortIDAsc
)

// FlowFilter defines filtering options for listing flows
type FlowFilter struct {
	UserID         string
	OrganizationID string
	// OrganizationIDs widens the org scope to multiple orgs; the service must verify membership before populating it — never bind directly from user input.
	OrganizationIDs []string
	Query           string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	Limit           int
	Offset          int
	// AfterID is a keyset cursor (only flows whose id sorts after it match), meaningful only with FlowSortIDAsc; backends that predate it ignore it — walkers must detect a non-advancing page.
	AfterID string
	SortBy  FlowSort
	// MetadataOnly skips loading each flow's Content; backends leave FlowDocument.Content empty when set.
	MetadataOnly bool
	// AllFlows is the operational-enumeration opt-in (the legacy migrator, the governance scanner) that bypasses owner/org scoping — a system read: never bind from user input (callers pinned by TestArch_SystemReadsPinned); without it an unscoped filter matches nothing.
	AllFlows bool
	// SharedOnly limits results to flows where the caller is a collaborator (not owner, not via org).
	SharedOnly bool
	// Tags matches flows carrying ANY of the listed tags (tag-picker any-of semantics); normalization is storage's concern.
	Tags []string
}

// ErrContentSearchUnsupported is returned by SearchFlowContents on backends that cannot push content matching into the store; callers fall back to their legacy scan.
var ErrContentSearchUnsupported = errors.New("content search not supported by this backend")

// FlowDocument represents a flow document
type FlowDocument struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Content     json.RawMessage `json:"content"`
	// Source is the raw PAD source (single-file flows) that cloud-mode apply/preview-fix patches line-based instead of Content; empty for legacy rows and multi-file flows.
	Source         string       `json:"source,omitempty"`
	Metadata       FlowMetadata `json:"metadata"`
	OwnerID        string       `json:"ownerId"`
	OrganizationID string       `json:"organizationId"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	Version        int          `json:"version"`
	// Tags are the flow's organizational labels, normalized by storage (see NormalizeFlowTags).
	Tags []string `json:"tags,omitempty"`
}

// FlowMetadata is stored as flows.metadata and READ BY SQL (ORDER BY metadata->>'BlockCount', dashboard sums, an expression index) — its JSON keys are a storage contract.
type FlowMetadata struct {
	BlockCount   int
	SubflowCount int
	MaxDepth     int
	ParsedAt     time.Time
	FileSize     int
	RawLineCount int
}
