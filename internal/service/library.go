package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// LibraryService manages CRUD operations for saved flows in cloud mode.
type LibraryService struct {
	storage storageif.StorageBackend
	mode    config.DeploymentMode
	// bus announces every flow the library creates, changes or deletes (FlowChanged); nil in bare tests.
	bus   *eventbus.Bus
	authz *AuthzService
	// scopes resolves what a library scope covers; the analytics dashboard lists through the same resolver.
	scopes *FlowScopeResolver
}

// NewLibraryService builds the library service; a nil scopes builds a resolver over the same storage and authz
// (it holds no state, so sharing DI's instance is wiring, not behaviour).
func NewLibraryService(storage storageif.StorageBackend, bus *eventbus.Bus, authz *AuthzService, scopes *FlowScopeResolver) *LibraryService {
	mode := config.ModeLocal
	if storage != nil {
		mode = config.ModeCloud
	}
	if scopes == nil {
		scopes = NewFlowScopeResolver(storage, authz)
	}
	return &LibraryService{storage: storage, mode: mode, bus: bus, authz: authz, scopes: scopes}
}

// authorize decides action on doc, a flow as storage holds it, for the actor ctx carries (AuthzService.Can, which
// also applies the action's API-token scope rule). The stored header decides, so no second read is made. All
// policy lives in AuthzService — owner, org membership, and collaborator grants all count. Cloud mode only.
func (s *LibraryService) authorize(ctx context.Context, action Action, doc *storageif.FlowDocument) error {
	return s.authz.Can(ctx, auth.ActorFromContext(ctx), action, FlowHeader(doc))
}

// CanRead reports whether the caller (the actor ctx carries) may read the flow: FlowRead.
func (s *LibraryService) CanRead(ctx context.Context, doc *storageif.FlowDocument) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authorize(ctx, FlowRead, doc) == nil
}

// CanWrite returns nil if the caller may edit the flow (FlowEdit), else why not — a write gate beyond the initial
// read check.
func (s *LibraryService) CanWrite(ctx context.Context, doc *storageif.FlowDocument) error {
	if s.mode == config.ModeLocal {
		return nil
	}
	return s.authorize(ctx, FlowEdit, doc)
}

// CanShareFlow reports whether the caller may manage who has access to the flow: FlowShare.
func (s *LibraryService) CanShareFlow(ctx context.Context, doc *storageif.FlowDocument) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authorize(ctx, FlowShare, doc) == nil
}

// CanDeleteFlow reports whether the caller may delete the flow: FlowDelete.
func (s *LibraryService) CanDeleteFlow(ctx context.Context, doc *storageif.FlowDocument) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authorize(ctx, FlowDelete, doc) == nil
}

// FlowPermissions returns the caller's edit/delete/share flags for one flow in
// a single pass (see AuthzService.FlowPermissions).
func (s *LibraryService) FlowPermissions(ctx context.Context, doc *storageif.FlowDocument, userID string) (canEdit, canDelete, canShare bool) {
	if s.mode == config.ModeLocal {
		return true, true, true
	}
	return s.authz.FlowPermissions(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, userID)
}

// BatchFlowPermissions resolves capability flags for all flows in O(orgs +
// 1) queries instead of O(flows). See AuthzService.BatchFlowPermissions.
func (s *LibraryService) BatchFlowPermissions(ctx context.Context, docs []*storageif.FlowDocument, userID string) map[string]PermFlags {
	if s.mode == config.ModeLocal {
		out := make(map[string]PermFlags, len(docs))
		for _, d := range docs {
			out[d.ID] = PermFlags{CanEdit: true, CanDelete: true, CanShare: true}
		}
		return out
	}
	return s.authz.BatchFlowPermissions(ctx, docs, userID)
}

// ResolveOwnerName returns the email of the owner.
func (s *LibraryService) ResolveOwnerName(ctx context.Context, ownerID string) string {
	if ownerID == "" || s.storage == nil {
		return ""
	}
	if u, err := s.storage.LoadUserByID(ctx, ownerID); err == nil {
		return u.Email
	}
	return ""
}

// ResolveOwnerNames resolves many owner IDs to emails in one backend round
// trip (no N+1); unknown/empty IDs are absent from the map.
func (s *LibraryService) ResolveOwnerNames(ctx context.Context, ownerIDs []string) map[string]string {
	names := make(map[string]string, len(ownerIDs))
	if s.storage == nil || len(ownerIDs) == 0 {
		return names
	}
	users, err := s.storage.LoadUsersByIDs(ctx, ownerIDs)
	if err != nil {
		return names
	}
	for id, u := range users {
		names[id] = u.Email
	}
	return names
}

// LibraryScope narrows which subset of flows the caller wants to see.
type LibraryScope string

const (
	// ScopeAll = everything visible to the caller (owned + collaborator-shared +
	// every org they're a member of). This is the default in the new library UI.
	ScopeAll LibraryScope = "all"
	// ScopeMine = flows the caller owns.
	ScopeMine LibraryScope = "mine"
	// ScopeShared = flows the caller is a collaborator on, but does not own.
	ScopeShared LibraryScope = "shared"
)

// ParseFlowSort maps the public sort param to a storage FlowSort. Unknown
// values fall back to updated_desc.
func ParseFlowSort(s string) storageif.FlowSort {
	switch s {
	case "updated_asc":
		return storageif.FlowSortUpdatedAsc
	case "name_asc":
		return storageif.FlowSortNameAsc
	case "name_desc":
		return storageif.FlowSortNameDesc
	case "blocks_desc":
		return storageif.FlowSortBlocksDesc
	default:
		return storageif.FlowSortUpdatedDesc
	}
}

// ListLibraryFlows returns the flows visible to the caller; an orgID requires
// membership (else enumeration by guessing). Scopes: all / mine / shared.
func (s *LibraryService) ListLibraryFlows(ctx context.Context, userID, orgID string, scope LibraryScope, query, sort string, limit, offset int) (docs []*storageif.FlowDocument, err error) {
	return s.ListLibraryFlowsWithTagFilter(ctx, userID, orgID, scope, query, sort, limit, offset, nil)
}

// ListLibraryFlowsWithTagFilter is ListLibraryFlows with a tag ANY-of filter;
// invalid tag names match nothing (storage rejects them always-false).
func (s *LibraryService) ListLibraryFlowsWithTagFilter(ctx context.Context, userID, orgID string, scope LibraryScope, query, sort string, limit, offset int, tags []string) (docs []*storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.ListLibraryFlows", &err)
	if s.mode == config.ModeLocal {
		return []*storageif.FlowDocument{}, nil
	}
	filter, _, err := s.scopes.filter(ctx, userID, orgID, scope, query, ParseFlowSort(sort), limit, offset)
	if err != nil {
		return nil, err
	}
	filter.Tags = tags
	return s.storage.ListFlows(ctx, filter)
}

// LibraryPage is one page of the library listing with the total under the same
// filter, plus the facts the per-row permission flags need again.
type LibraryPage struct {
	Docs  []*storageif.FlowDocument
	Total int
	// Memberships is the caller's complete org-membership list when scope "all"
	// resolved it (an absent org = not a member); nil when unused. Feed the flags call.
	Memberships []storageif.OrgMembership
}

// ListLibraryFlowsPage is list + count in one call: the scope — membership
// query included — resolves ONCE and one filter serves page and total.
func (s *LibraryService) ListLibraryFlowsPage(ctx context.Context, userID, orgID string, scope LibraryScope, query, sort string, limit, offset int, tags []string) (page *LibraryPage, err error) {
	defer logger.Guard("LibraryService.ListLibraryFlowsPage", &err)
	if s.mode == config.ModeLocal {
		return &LibraryPage{Docs: []*storageif.FlowDocument{}}, nil
	}
	filter, memberships, err := s.scopes.filter(ctx, userID, orgID, scope, query, ParseFlowSort(sort), limit, offset)
	if err != nil {
		return nil, err
	}
	filter.Tags = tags
	// Page and total in one statement: the visibility filter (membership
	// semi-join + RLS) is the read path's most expensive predicate.
	docs, total, err := s.storage.ListFlowsWithTotal(ctx, filter)
	if err != nil {
		return nil, err
	}
	return &LibraryPage{Docs: docs, Total: total, Memberships: memberships}, nil
}

// BatchFlowPermissionsWithRoles is BatchFlowPermissions seeded with the
// caller's memberships — with a complete list no per-org role query runs.
func (s *LibraryService) BatchFlowPermissionsWithRoles(ctx context.Context, docs []*storageif.FlowDocument, userID string, memberships []storageif.OrgMembership) map[string]PermFlags {
	if s.mode == config.ModeLocal {
		return s.BatchFlowPermissions(ctx, docs, userID)
	}
	return s.authz.BatchFlowPermissionsWithRoles(ctx, docs, userID, memberships)
}

// CountLibraryFlows returns the total visible under the filter, ignoring
// pagination; same org membership rule as ListLibraryFlows.
func (s *LibraryService) CountLibraryFlows(ctx context.Context, userID, orgID string, scope LibraryScope, query string) (total int, err error) {
	return s.CountLibraryFlowsWithTagFilter(ctx, userID, orgID, scope, query, nil)
}

// CountLibraryFlowsWithTagFilter is CountLibraryFlows with a tag ANY-of
// filter — the total must be computed under the SAME filter the list used.
func (s *LibraryService) CountLibraryFlowsWithTagFilter(ctx context.Context, userID, orgID string, scope LibraryScope, query string, tags []string) (total int, err error) {
	defer logger.Guard("LibraryService.CountLibraryFlows", &err)
	if s.mode == config.ModeLocal {
		return 0, nil
	}
	filter, _, err := s.scopes.filter(ctx, userID, orgID, scope, query, storageif.FlowSortUpdatedDesc, 0, 0)
	if err != nil {
		return 0, err
	}
	filter.Tags = tags
	return s.storage.CountFlows(ctx, filter)
}

// FlowHealth returns the latest persisted analysis snapshot for a flow, or nil
// if never analyzed; the caller owns read authorization.
func (s *LibraryService) FlowHealth(ctx context.Context, flowID string) (h *storageif.HealthSnapshot, err error) {
	defer logger.Guard("LibraryService.FlowHealth", &err)
	if s.mode == config.ModeLocal || s.storage == nil {
		return nil, nil
	}
	return s.storage.LoadFlowHealth(ctx, flowID)
}

// CheckFlowPermission reports whether the caller (the actor ctx carries) may perform action on the flow
// (AuthzService.Can) WITHOUT loading its content — perm-only endpoints skip the blob the full load would pay to
// discard.
func (s *LibraryService) CheckFlowPermission(ctx context.Context, flowID string, action Action) error {
	if s.mode == config.ModeLocal || s.storage == nil {
		return nil
	}
	if s.authz == nil {
		return ErrPermissionDenied
	}
	return s.authz.Can(ctx, auth.ActorFromContext(ctx), action, FlowByID(flowID))
}

// GetLibraryFlow loads a single flow by ID without an access check.
//
// Deprecated: Use GetLibraryFlowForUser instead. This method bypasses all
// authorization and should only be called from within service-level code that
// performs its own authz check. Handler code must never call this directly.
func (s *LibraryService) GetLibraryFlow(ctx context.Context, flowID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, storageif.ErrNotFound
	}
	return s.storage.LoadFlow(ctx, flowID)
}

// GetLibraryFlowMetaForUser is GetLibraryFlowForUser reading only the header —
// no content, no blob (Content nil, Source empty); same read authorization.
func (s *LibraryService) GetLibraryFlowMetaForUser(ctx context.Context, flowID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlowMetaForUser", &err)
	if s.mode == config.ModeLocal {
		return nil, storageif.ErrNotFound
	}
	doc, err = s.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, FlowRead, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// GetLibraryFlowForUser loads a flow the caller (the actor ctx carries) may
// read, else Can's refusal — the handler form of GetLibraryFlow + CanRead.
func (s *LibraryService) GetLibraryFlowForUser(ctx context.Context, flowID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlowForUser", &err)
	doc, err = s.GetLibraryFlow(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, FlowRead, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// CreateLibraryFlow persists a new flow owned by ownerID; an org flow requires
// the caller to create content in the org (OrgWrite: admins and members, not
// viewers, guests or non-members).
func (s *LibraryService) CreateLibraryFlow(ctx context.Context, ownerID, orgID string, doc storageif.FlowDocument) (saved *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.CreateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, fmt.Errorf("cloud storage not available in local mode")
	}
	if orgID != "" {
		if err := s.authz.Can(ctx, auth.ActorFromContext(ctx), OrgWrite, OrgByID(orgID)); err != nil {
			return nil, err
		}
	}

	if doc.ID == "" {
		doc.ID = uuid.NewString()
	}

	doc.OwnerID = ownerID
	doc.OrganizationID = orgID
	if err := s.storage.SaveFlow(ctx, &doc); err != nil {
		return nil, err
	}
	s.publish(ctx, events.FlowCreated, &doc)
	return &doc, nil
}

// publish announces a change of doc, the flow as storage holds it after the change (inside the request's
// transaction when there is one: the bus ties the later deliveries to its outcome).
func (s *LibraryService) publish(ctx context.Context, kind events.FlowChangeKind, doc *storageif.FlowDocument) {
	version := int64(doc.Version)
	if kind == events.FlowDeleted {
		version = 0
	}
	s.bus.Publish(ctx, events.FlowChanged{
		FlowID: doc.ID, StableID: doc.ID, OwnerID: doc.OwnerID, OrgID: doc.OrganizationID, Kind: kind, Version: version,
	})
}

// UpdateLibraryFlow saves changes to an existing flow, doc as loaded from
// storage (its owner and organisation decide); the caller must be allowed
// FlowEdit. Flows with an empty OwnerID are unclaimed and read-only.
func (s *LibraryService) UpdateLibraryFlow(ctx context.Context, doc *storageif.FlowDocument) (err error) {
	defer logger.Guard("LibraryService.UpdateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return fmt.Errorf("cloud storage not available in local mode")
	}
	if err := s.authorize(ctx, FlowEdit, doc); err != nil {
		return err
	}
	if err := s.storage.SaveFlow(ctx, doc); err != nil {
		return err
	}
	s.publish(ctx, events.FlowUpdated, doc)
	return nil
}

// DeleteLibraryFlow removes a flow — FlowDelete, narrower than FlowShare: only
// the owner or an org admin may destroy — and returns the header it decided on
// (the audit row takes the flow's organisation from it). ErrNotFound in local mode.
func (s *LibraryService) DeleteLibraryFlow(ctx context.Context, flowID string) (deleted *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.DeleteLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, ErrNotFound
	}
	// Header only: the decision needs owner/org; the old full load downloaded
	// content just to discard it (and refused when the blob was missing).
	doc, err := s.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, FlowDelete, doc); err != nil {
		return nil, err
	}
	if err := s.storage.DeleteFlow(ctx, flowID); err != nil {
		return nil, err
	}
	s.publish(ctx, events.FlowDeleted, doc)
	return doc, nil
}

// ListFlowVersions returns up to limit of the flow's stored versions (limit <= 0 = all); the caller owns read
// authorization.
func (s *LibraryService) ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*storageif.FlowVersion, error) {
	return s.storage.ListFlowVersions(ctx, flowID, limit)
}

// LoadFlowVersion reads one stored version, ErrNotFound when it does not exist; the caller owns authorization.
func (s *LibraryService) LoadFlowVersion(ctx context.Context, flowID string, version int) (*storageif.FlowVersion, error) {
	return s.storage.LoadFlowVersion(ctx, flowID, version)
}

// SaveFlowVersion snapshots doc, the flow flowID as loaded, as a new version by userID and returns it with the
// number the store assigned; the caller owns the write check (CanWrite).
func (s *LibraryService) SaveFlowVersion(ctx context.Context, flowID string, doc *storageif.FlowDocument, comment, userID string) (*storageif.FlowVersion, error) {
	// The backend assigns v.Version atomically (Postgres: FOR UPDATE + MAX+1);
	// a read-then-write here would just race.
	v := &storageif.FlowVersion{
		ID:        uuid.NewString(),
		FlowID:    flowID,
		Comment:   comment,
		Content:   doc.Content,
		Metadata:  doc.Metadata,
		CreatedBy: userID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.storage.SaveFlowVersion(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}
