package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"pad-analyzer/internal/config"
	"pad-core/analyzer"
	"pad-core/logger"
	storageif "pad-analyzer/internal/storage/interfaces"
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

// CanWrite returns nil if userID has at least "editor" access to the flow.
// Returns ErrPermissionDenied otherwise. Used by handlers that need a write
// gate beyond the initial read check (e.g. version creation).
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

// FlowPermissions returns the caller's edit/delete/share capability flags for a
// single flow in one pass (see AuthzService.FlowPermissions). Used to populate
// the library list DTO without one CheckFlowAccess call per flag.
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

// ResolveOwnerNames resolves many owner IDs to display names (emails) in a
// single backend round trip, avoiding the N+1 pattern when building list
// responses. Unknown/empty IDs are simply absent from the returned map.
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
// drift. When scope=all and no specific orgID is supplied, the caller's full
// org-membership list is folded into the filter so the result spans every org
// the user belongs to.
func (s *LibraryService) buildLibraryFilter(ctx context.Context, userID, orgID string, scope LibraryScope, query string, sort storageif.FlowSort, limit, offset int) (storageif.FlowFilter, error) {
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
			return filter, err
		}
		filter.OrganizationID = orgID
		return filter, nil
	}
	switch scope {
	case ScopeMine:
		// userID only — no org widening, no collaborator inclusion via OR-org
		// (collaborator subquery still fires from the UserID branch).
		return filter, nil
	case ScopeShared:
		filter.SharedOnly = true
		return filter, nil
	case "", ScopeAll:
		// Widen org scope to every org the user belongs to. ListOrgsForUser is
		// the authoritative membership source — handlers must never bind these
		// IDs from user input.
		orgs, err := s.storage.ListOrgsForUser(ctx, userID)
		if err != nil {
			return filter, err
		}
		ids := make([]string, 0, len(orgs))
		for _, o := range orgs {
			ids = append(ids, o.ID)
		}
		filter.OrganizationIDs = ids
		return filter, nil
	default:
		return filter, nil
	}
}

// ListLibraryFlows returns flows visible to the requesting user. When orgID
// is set the caller must be a member of that org — otherwise any
// authenticated user could enumerate another org's flows by guessing its ID.
//
// When orgID is empty, `scope` controls breadth:
//   - "all" (default) — owned + collaborator-shared + every org the user belongs to
//   - "mine" — only flows the user owns
//   - "shared" — only flows shared with the user (excluding owned)
func (s *LibraryService) ListLibraryFlows(ctx context.Context, userID, orgID string, scope LibraryScope, query, sort string, limit, offset int) (docs []*storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.ListLibraryFlows", &err)
	if s.mode == config.ModeLocal {
		return []*storageif.FlowDocument{}, nil
	}
	filter, err := s.buildLibraryFilter(ctx, userID, orgID, scope, query, ParseFlowSort(sort), limit, offset)
	if err != nil {
		return nil, err
	}
	return s.storage.ListFlows(ctx, filter)
}

// CountLibraryFlows returns the total number of flows visible to the user for
// the given filter, ignoring pagination — used for list totals. Same org
// membership rule as ListLibraryFlows.
func (s *LibraryService) CountLibraryFlows(ctx context.Context, userID, orgID string, scope LibraryScope, query string) (total int, err error) {
	defer logger.Guard("LibraryService.CountLibraryFlows", &err)
	if s.mode == config.ModeLocal {
		return 0, nil
	}
	filter, err := s.buildLibraryFilter(ctx, userID, orgID, scope, query, storageif.FlowSortUpdatedDesc, 0, 0)
	if err != nil {
		return 0, err
	}
	return s.storage.CountFlows(ctx, filter)
}

// FlowHealth returns the latest persisted analysis snapshot for a flow, or nil
// if the flow has never been analyzed. The caller is responsible for read
// authorization (typically by going through GetLibraryFlowForUser first).
func (s *LibraryService) FlowHealth(ctx context.Context, flowID string) (h *storageif.HealthSnapshot, err error) {
	defer logger.Guard("LibraryService.FlowHealth", &err)
	if s.mode == config.ModeLocal || s.storage == nil {
		return nil, nil
	}
	return s.storage.LoadFlowHealth(ctx, flowID)
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

// GetLibraryFlowForUser loads a flow and returns ErrPermissionDenied if userID
// does not have read access. Use this in handlers instead of calling GetLibraryFlow
// followed by a separate CanRead check.
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

// CreateLibraryFlow persists a new flow owned by ownerID. Creating a flow
// inside an org requires an org role of at least member (viewers/guests and
// non-members may not write into the org's library).
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

// UpdateLibraryFlow saves changes to an existing flow. Requires at least
// "editor" access: owner, org admin/member, or a collaborator with the
// editor/admin tier. Flows with an empty OwnerID are unclaimed and read-only.
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

// DeleteLibraryFlow removes a flow. Deletion is narrower than the "admin"
// permission rank: only the owner or an org admin may destroy a flow.
// Returns ErrNotFound in local mode.
func (s *LibraryService) DeleteLibraryFlow(ctx context.Context, flowID, callerID string) (err error) {
	defer logger.Guard("LibraryService.DeleteLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return ErrNotFound
	}
	doc, err := s.storage.LoadFlow(ctx, flowID)
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
	}
	return nil
}
