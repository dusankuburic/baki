package interfaces

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
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
	// SearchFlowContents returns METADATA-ONLY documents whose content mentions needle, newest first, in the filter's scope; backends without a queryable content column return ErrContentSearchUnsupported, the others ErrQueryTooShort for a needle ShortSearchTerm calls short (Postgres: or its database draws no trigram from).
	SearchFlowContents(ctx context.Context, filter FlowFilter, needle string, limit int) ([]*FlowDocument, error)
	LoadFlow(ctx context.Context, id string) (*FlowDocument, error)
	// LoadFlowHeader returns a flow's metadata WITHOUT content, so authorization/existence/version checks avoid the content fetch and blob availability.
	LoadFlowHeader(ctx context.Context, id string) (*FlowDocument, error)
	ListFlows(ctx context.Context, filter FlowFilter) ([]*FlowDocument, error)
	// CountFlows returns the total matching the filter, ignoring Limit/Offset (list pagination totals).
	CountFlows(ctx context.Context, filter FlowFilter) (int, error)
	// ListFlowsPage is ListFlows' page with whether more flows follow it, and how many the filter matches without the page bounds (Offset, After), counted up to totalCap (more: TotalCapped; totalCap <= 0 counts all); a page past the end still reports the total, and a keyset page whose cursor is Counted reports the cursor's total without counting.
	ListFlowsPage(ctx context.Context, filter FlowFilter, totalCap int) (*FlowPage, error)
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
	// After is a keyset cursor for FlowSortUpdatedDesc and FlowSortUpdatedAsc (the UpdatedAt and ID of the last flow of the page before): only the flows after it in that order match, and Offset is ignored (the page starts After.Skip flows after the position; CountFlows counts them). With another sort it matches nothing.
	After  *PageCursor
	SortBy FlowSort
	// MetadataOnly skips loading each flow's Content; backends leave FlowDocument.Content empty when set.
	MetadataOnly bool
	// AllFlows is the operational-enumeration opt-in (the legacy migrator, the governance scanner) that bypasses owner/org scoping — a system read: never bind from user input (callers pinned by TestArch_SystemReadsPinned); without it an unscoped filter matches nothing.
	AllFlows bool
	// SharedOnly limits results to flows where the caller is a collaborator (not owner, not via org).
	SharedOnly bool
	// Tags matches flows carrying ANY of the listed tags (tag-picker any-of semantics); normalization is storage's concern.
	Tags []string
	// QueryPrefix matches Query as a prefix of the name even when ShortSearchTerm calls it long: the Postgres store
	// sets it for a term its database draws no trigram from.
	QueryPrefix bool
}

// FlowPage is one ListFlowsPage page. Total is the number of flows the filter matches, or the cap when TotalCapped
// (more match than that).
type FlowPage struct {
	Docs        []*FlowDocument
	HasMore     bool
	Total       int
	TotalCapped bool
}

// ErrContentSearchUnsupported is returned by SearchFlowContents on backends that cannot push content matching into the store; callers fall back to their legacy scan.
var ErrContentSearchUnsupported = errors.New("content search not supported by this backend")

// MinSearchTermRunes is the shortest run of word characters (SearchWordChar) a search term needs: pg_trgm draws a
// LIKE pattern's trigrams from such runs only (punctuation and spaces split words), and a run shorter than this
// inside the pattern gives none, so a content search without one would read every tenant's flow text, whatever the
// term's length ("---", "a b"). SearchFlowContents refuses one (ErrQueryTooShort); FlowFilter.Query, otherwise a
// case-insensitive substring of the name, matches a prefix of it instead (a blank Query does not filter).
const MinSearchTermRunes = 3

// ErrQueryTooShort is SearchFlowContents' answer to a non-blank term without a run of MinSearchTermRunes word
// characters.
var ErrQueryTooShort = errors.New("search term too short")

// SearchWordChar reports whether r is a word character to pg_trgm in a database with a UTF-8 ctype, which asks
// glibc's iswalnum: a letter, a letter number, a mark Unicode calls alphabetic (the vowel signs inside Devanagari,
// Tamil, Thai or Arabic words) or a decimal digit of any script. The database may still count fewer: a C ctype
// counts ASCII letters and digits only, and its C library may predate letters Go's tables know. The web client
// counts the same (frontend/src/lib/searchTerm.ts).
func SearchWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.Nl, unicode.Other_Alphabetic)
}

// ShortSearchTerm reports whether term, trimmed, is non-blank and has no run of MinSearchTermRunes word characters
// (SearchWordChar). A term it calls long may still be short to the database; the Postgres store asks it.
func ShortSearchTerm(term string) bool {
	term = strings.TrimSpace(term)
	if term == "" {
		return false
	}
	run := 0
	for _, r := range term {
		if !SearchWordChar(r) {
			run = 0
			continue
		}
		if run++; run >= MinSearchTermRunes {
			return false
		}
	}
	return true
}

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
