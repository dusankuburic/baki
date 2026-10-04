package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
)

// LibraryService manages CRUD operations for saved flows in cloud mode.
type LibraryService struct {
	storage   storageif.StorageBackend
	mode      config.DeploymentMode
	flowCache *FlowService // Needed to invalidate search index when flow is updated/deleted
	authz     *AuthzService
}

func NewLibraryService(storage storageif.StorageBackend, flowCache *FlowService, authz *AuthzService) *LibraryService {
	mode := config.ModeLocal
	if storage != nil {
		mode = config.ModeCloud
	}
	return &LibraryService{storage: storage, mode: mode, flowCache: flowCache, authz: authz}
}

// CanRead reports whether userID may read the flow. All policy lives in
// AuthzService — owner, org membership, and collaborator grants all count.
func (s *LibraryService) CanRead(ctx context.Context, doc *storageif.FlowDocument, userID string) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authz.CheckFlowAccess(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, userID, "viewer") == nil
}

// CanWrite returns nil if userID has at least "editor" access, else
// ErrPermissionDenied — a write gate beyond the initial read check.
func (s *LibraryService) CanWrite(ctx context.Context, doc *storageif.FlowDocument, userID string) error {
	if s.mode == config.ModeLocal {
		return nil
	}
	return s.authz.CheckFlowAccess(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, userID, "editor")
}

func (s *LibraryService) CanShareFlow(ctx context.Context, doc *storageif.FlowDocument, userID string) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authz.CheckFlowAccess(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, userID, "admin") == nil
}

func (s *LibraryService) CanDeleteFlow(ctx context.Context, doc *storageif.FlowDocument, userID string) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	return s.authz.CanDeleteFlow(ctx, doc.OwnerID, doc.OrganizationID, userID)
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

// buildLibraryFilter centralises scope/org resolution so List and Count cannot
// drift; scope=all without orgID also returns the caller's folded membership list.
func (s *LibraryService) buildLibraryFilter(ctx context.Context, userID, orgID string, scope LibraryScope, query string, sort storageif.FlowSort, limit, offset int) (storageif.FlowFilter, []storageif.OrgMembership, error) {
	filter := storageif.FlowFilter{
		UserID:       userID,
		Query:        query,
		Limit:        limit,
		Offset:       offset,
		SortBy:       sort,
		MetadataOnly: true,
	}
	if orgID != "" {
		if err := s.authz.RequireOrgMember(ctx, orgID, userID); err != nil {
			return filter, nil, err
		}
		filter.OrganizationID = orgID
		return filter, nil, nil
	}
	switch scope {
	case ScopeMine:
		// userID only — no org widening, no collaborator inclusion via OR-org
		// (collaborator subquery still fires from the UserID branch).
		return filter, nil, nil
	case ScopeShared:
		filter.SharedOnly = true
		return filter, nil, nil
	case "", ScopeAll:
		// Widen org scope to every org the user belongs to; the storage
		// membership list is authoritative — never bound from user input.
		ms, err := s.memberOrgs(ctx, userID)
		if err != nil {
			return filter, nil, err
		}
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.OrgID)
		}
		filter.OrganizationIDs = ids
		return filter, ms, nil
	default:
		return filter, nil, nil
	}
}

// memberOrgs lists the orgs userID belongs to with the role in each (the
// roster-free list when the backend has one). Never nil — empty means "no orgs".
func (s *LibraryService) memberOrgs(ctx context.Context, userID string) ([]storageif.OrgMembership, error) {
	if l, ok := s.storage.(storageif.OrgMembershipLister); ok {
		ms, err := l.ListOrgMembershipsForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
		if ms == nil {
			ms = []storageif.OrgMembership{}
		}
		return ms, nil
	}
	orgs, err := s.storage.ListOrgsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ms := make([]storageif.OrgMembership, 0, len(orgs))
	for _, o := range orgs {
		m := storageif.OrgMembership{OrgID: o.ID, OrgName: o.Name}
		for _, om := range o.Members {
			if om.UserID == userID {
				m.Role = om.Role
				break
			}
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// ScopeFlowIDs resolves the flow ids the caller may see under a scope, paged
// in 500s up to limit; result two lists the widening org ids (org-keyed analytics).
func (s *LibraryService) ScopeFlowIDs(ctx context.Context, userID, orgID string, scope LibraryScope, limit int) (ids []string, orgIDs []string, err error) {
	defer logger.Guard("LibraryService.ScopeFlowIDs", &err)
	ids, orgIDs = []string{}, []string{}
	if s.mode == config.ModeLocal || s.storage == nil {
		return ids, orgIDs, nil
	}
	if limit <= 0 {
		limit = 5000
	}
	const page = 500
	// The scope (membership check, org list) is resolved ONCE for the walk;
	// it used to be rebuilt — membership queries included — for every page.
	filter, _, ferr := s.buildLibraryFilter(ctx, userID, orgID, scope, "", storageif.FlowSortIDAsc, page, 0)
	if ferr != nil {
		return nil, nil, ferr
	}
	if filter.OrganizationID != "" {
		orgIDs = []string{filter.OrganizationID}
	} else if len(filter.OrganizationIDs) > 0 {
		orgIDs = append(orgIDs, filter.OrganizationIDs...)
	}
	// Keyset paging on the id order: each page starts after the last id seen
	// (AfterID); a backend without AfterID repeats page one — detected, OFFSET takes over.
	keyset := true
	seen := make(map[string]struct{})
	for len(ids) < limit {
		if keyset && len(ids) > 0 {
			filter.AfterID = ids[len(ids)-1]
		}
		if !keyset {
			filter.AfterID = ""
			filter.Offset = len(ids)
		}
		docs, lerr := s.storage.ListFlows(ctx, filter)
		if lerr != nil {
			return nil, nil, lerr
		}
		if keyset && filter.AfterID != "" && len(docs) > 0 {
			if _, dup := seen[docs[0].ID]; dup {
				keyset = false
				continue
			}
		}
		for _, d := range docs {
			ids = append(ids, d.ID)
			seen[d.ID] = struct{}{}
			if len(ids) >= limit {
				break
			}
		}
		if len(docs) < page {
			break
		}
	}
	return ids, orgIDs, nil
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
	filter, _, err := s.buildLibraryFilter(ctx, userID, orgID, scope, query, ParseFlowSort(sort), limit, offset)
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
	filter, memberships, err := s.buildLibraryFilter(ctx, userID, orgID, scope, query, ParseFlowSort(sort), limit, offset)
	if err != nil {
		return nil, err
	}
	filter.Tags = tags
	// One statement when the backend can: the visibility filter (membership
	// semi-join + RLS) is the read path's most expensive predicate.
	if lp, ok := s.storage.(storageif.FlowPageLister); ok {
		docs, total, err := lp.ListFlowsWithTotal(ctx, filter)
		if err != nil {
			return nil, err
		}
		return &LibraryPage{Docs: docs, Total: total, Memberships: memberships}, nil
	}
	docs, err := s.storage.ListFlows(ctx, filter)
	if err != nil {
		return nil, err
	}
	// The total ignores pagination: same filter, no limit/offset/sort.
	countFilter := filter
	countFilter.Limit, countFilter.Offset, countFilter.SortBy = 0, 0, storageif.FlowSortUpdatedDesc
	total, err := s.storage.CountFlows(ctx, countFilter)
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
	filter, _, err := s.buildLibraryFilter(ctx, userID, orgID, scope, query, storageif.FlowSortUpdatedDesc, 0, 0)
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

// CheckFlowAccessForUser verifies access WITHOUT loading the flow's content —
// perm-only endpoints skip the blob the full load would pay to discard.
func (s *LibraryService) CheckFlowAccessForUser(ctx context.Context, flowID, userID, minPerm string) error {
	if s.mode == config.ModeLocal || s.storage == nil {
		return nil
	}
	if s.authz == nil {
		return ErrPermissionDenied
	}
	return s.authz.CheckFlowAccessByID(ctx, flowID, userID, minPerm)
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
func (s *LibraryService) GetLibraryFlowMetaForUser(ctx context.Context, flowID, userID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlowMetaForUser", &err)
	if s.mode == config.ModeLocal {
		return nil, storageif.ErrNotFound
	}
	doc, err = s.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if !s.CanRead(ctx, doc, userID) {
		return nil, ErrPermissionDenied
	}
	return doc, nil
}

// GetLibraryFlowForUser loads a flow and returns ErrPermissionDenied without
// read access — the handler form of GetLibraryFlow + CanRead.
func (s *LibraryService) GetLibraryFlowForUser(ctx context.Context, flowID, userID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlowForUser", &err)
	doc, err = s.GetLibraryFlow(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if !s.CanRead(ctx, doc, userID) {
		return nil, ErrPermissionDenied
	}
	return doc, nil
}

// CreateLibraryFlow persists a new flow owned by ownerID; an org flow requires
// org role at least member (viewers/guests/non-members may not write).
func (s *LibraryService) CreateLibraryFlow(ctx context.Context, ownerID, orgID string, doc storageif.FlowDocument) (saved *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.CreateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, fmt.Errorf("cloud storage not available in local mode")
	}
	if orgID != "" {
		if err := s.authz.RequireOrgWriter(ctx, orgID, ownerID); err != nil {
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
	return &doc, nil
}

// UpdateLibraryFlow saves changes to an existing flow; requires "editor"
// access. Flows with an empty OwnerID are unclaimed and read-only.
func (s *LibraryService) UpdateLibraryFlow(ctx context.Context, doc *storageif.FlowDocument, callerID string) (err error) {
	defer logger.Guard("LibraryService.UpdateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return fmt.Errorf("cloud storage not available in local mode")
	}
	if err := s.authz.CheckFlowAccess(ctx, doc.ID, doc.OwnerID, doc.OrganizationID, callerID, "editor"); err != nil {
		return err
	}
	if err := s.storage.SaveFlow(ctx, doc); err != nil {
		return err
	}
	// Content changed in place (same ID) — drop any cached search index
	if s.flowCache != nil {
		s.flowCache.InvalidateSearchIndex(doc.ID)
	}
	return nil
}

// DeleteLibraryFlow removes a flow — narrower than "admin": only the owner or
// an org admin may destroy. ErrNotFound in local mode.
func (s *LibraryService) DeleteLibraryFlow(ctx context.Context, flowID, callerID string) (err error) {
	defer logger.Guard("LibraryService.DeleteLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return ErrNotFound
	}
	// Header only: the decision needs owner/org; the old full load downloaded
	// content just to discard it (and refused when the blob was missing).
	doc, err := s.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return err
	}
	if !s.authz.CanDeleteFlow(ctx, doc.OwnerID, doc.OrganizationID, callerID) {
		return ErrPermissionDenied
	}
	if err := s.storage.DeleteFlow(ctx, flowID); err != nil {
		return err
	}
	analyzer.DefaultCache.Invalidate(flowID)
	if s.flowCache != nil {
		s.flowCache.InvalidateSearchIndex(flowID)
		// The resolved-document cache pins on (id, version); a row that
		// later reappears under the same id must not be served the old parse.
		if p, ok := s.flowCache.DocProvider().(interface{ Invalidate(string) }); ok {
			p.Invalidate(flowID)
		}
	}
	return nil
}
