package service

import (
	"context"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// AuthzService is the single source of truth for flow- and org-level
// authorization decisions in cloud mode. All services and handlers that gate
// access to tenant-scoped resources must delegate here — do not duplicate
// policy in handlers or other services.
//
// In local/Tauri mode (storage == nil) every check allows: the desktop app is
// single-user and the API is already gated by the static sidecar token.
type AuthzService struct {
	storage storageif.StorageBackend
	orgSvc  *collaboration.OrgService
}

func NewAuthzService(storage storageif.StorageBackend, orgSvc *collaboration.OrgService) *AuthzService {
	if storage != nil && orgSvc == nil {
		logger.Warn("AuthzService created in cloud mode (storage != nil) but orgSvc is nil — org-based access control is disabled")
	}
	return &AuthzService{storage: storage, orgSvc: orgSvc}
}

// CheckFlowAccess reports whether userID has at least minPerm access to the
// flow identified by flowID with the given owner/org. minPerm is "viewer",
// "editor", or "admin". Returns nil when allowed, ErrPermissionDenied otherwise.
//
// Access is granted by, in order: ownership, org role (admin→admin,
// member→editor, viewer/guest→viewer), or an explicit collaborator grant.
// Flows with an empty OwnerID are legacy/unclaimed and are readable by any
// authenticated user but never writable.
func (a *AuthzService) CheckFlowAccess(ctx context.Context, flowID, ownerID, orgID, userID, minPerm string) error {
	if a.storage == nil { // Local mode
		return nil
	}

	need := permRank(minPerm)
	if need == 0 {
		// Unrecognised minimum-permission requirement: fail closed rather than
		// treating it as "no requirement" (which, via the >= need comparisons
		// below, would otherwise grant any collaborator).
		return ErrPermissionDenied
	}

	// Ownerless legacy flows: read-only for everyone.
	if ownerID == "" {
		if need <= permRank("viewer") {
			return nil
		}
		return ErrPermissionDenied
	}

	if ownerID == userID {
		return nil
	}

	// Org role grants tiered access to org flows. A non-member (ErrMemberNotFound)
	// or a transient lookup error must NOT short-circuit the explicit collaborator
	// grant below — org membership is one access path, not a gate. Grant here only
	// on a successful lookup meeting the threshold; otherwise fall through (the
	// trailing return fail-closes), mirroring FlowPermissions/BatchFlowPermissions.
	if orgID != "" && a.orgSvc != nil {
		if role, err := a.orgSvc.MemberRole(ctx, orgID, userID); err == nil && orgRoleToPermRank(role) >= need {
			return nil
		}
	}

	// Explicit per-flow collaborator grant.
	collabs, err := a.storage.ListCollaborators(ctx, flowID)
	if err != nil {
		return ErrPermissionDenied
	}
	for _, c := range collabs {
		if c.UserID == userID && permRank(c.Permission) >= need {
			return nil
		}
	}

	return ErrPermissionDenied
}

// CheckFlowAccessByID loads the flow header from storage and delegates to
// CheckFlowAccess. Propagates storage errors (notably storageif.ErrNotFound)
// so callers can distinguish 404 from 403.
func (a *AuthzService) CheckFlowAccessByID(ctx context.Context, flowID, userID, minPerm string) error {
	if a.storage == nil { // Local mode
		return nil
	}
	doc, err := a.storage.LoadFlow(ctx, flowID)
	if err != nil {
		return err
	}
	return a.CheckFlowAccess(ctx, flowID, doc.OwnerID, doc.OrganizationID, userID, minPerm)
}

// CanDeleteFlow reports whether userID may delete the flow. Deletion is
// deliberately narrower than the "admin" permission rank: only the owner or
// an org admin may destroy a flow — a collaborator with the "admin" tier can
// manage sharing but not delete.
func (a *AuthzService) CanDeleteFlow(ctx context.Context, ownerID, orgID, userID string) bool {
	if a.storage == nil { // Local mode
		return true
	}
	if ownerID != "" && ownerID == userID {
		return true
	}
	if orgID != "" && a.orgSvc != nil {
		return a.orgSvc.IsAdmin(ctx, orgID, userID)
	}
	return false
}

// FlowPermissions computes the editor/share/delete capability flags for userID
// against a single flow in one pass, issuing at most one org-role lookup and one
// collaborator lookup — and none at all for the owner or an ownerless flow. It
// exists so list endpoints can populate per-row capability flags without making
// a separate CheckFlowAccess call per flag (which multiplies per-row queries).
//
// canDelete mirrors CanDeleteFlow: only the owner or an org admin may delete,
// never an admin-tier collaborator.
func (a *AuthzService) FlowPermissions(ctx context.Context, flowID, ownerID, orgID, userID string) (canEdit, canDelete, canShare bool) {
	if a.storage == nil { // Local mode
		return true, true, true
	}
	// Ownerless legacy flows are readable by all but writable by none.
	if ownerID == "" {
		return false, false, false
	}
	if ownerID == userID {
		return true, true, true
	}

	rank := 0
	isOrgAdmin := false
	if orgID != "" && a.orgSvc != nil {
		if role, err := a.orgSvc.MemberRole(ctx, orgID, userID); err == nil {
			if r := orgRoleToPermRank(role); r > rank {
				rank = r
			}
			isOrgAdmin = role == auth.RoleAdmin
		}
	}
	// Only consult collaborators if the org role hasn't already granted top tier.
	if rank < permRank("admin") {
		if collabs, err := a.storage.ListCollaborators(ctx, flowID); err == nil {
			for _, c := range collabs {
				if c.UserID == userID {
					if r := permRank(c.Permission); r > rank {
						rank = r
					}
				}
			}
		}
	}

	canEdit = rank >= permRank("editor")
	canShare = rank >= permRank("admin")
	canDelete = isOrgAdmin // the owner case already returned above
	return
}

// PermFlags holds the per-flow capability flags computed by BatchFlowPermissions.
type PermFlags struct {
	CanEdit, CanDelete, CanShare bool
}

// BatchFlowPermissions resolves capability flags for multiple flows in at most
// 1 + N(distinctOrgs) + 1 queries instead of 3*N. It caches org-role lookups
// per distinct org and fetches all collaborators in a single batch query.
func (a *AuthzService) BatchFlowPermissions(ctx context.Context, docs []*storageif.FlowDocument, userID string) map[string]PermFlags {
	result := make(map[string]PermFlags, len(docs))
	if a.storage == nil {
		for _, d := range docs {
			result[d.ID] = PermFlags{CanEdit: true, CanDelete: true, CanShare: true}
		}
		return result
	}

	orgRoleCache := make(map[string]auth.Role)
	needCollabs := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.OwnerID == "" {
			result[d.ID] = PermFlags{}
			continue
		}
		if d.OwnerID == userID {
			result[d.ID] = PermFlags{CanEdit: true, CanDelete: true, CanShare: true}
			continue
		}
		rank := 0
		isOrgAdmin := false
		if d.OrganizationID != "" && a.orgSvc != nil {
			role, cached := orgRoleCache[d.OrganizationID]
			if !cached {
				if r, err := a.orgSvc.MemberRole(ctx, d.OrganizationID, userID); err == nil {
					role = r
				}
				orgRoleCache[d.OrganizationID] = role
			}
			if r := orgRoleToPermRank(role); r > rank {
				rank = r
			}
			isOrgAdmin = role == auth.RoleAdmin
		}
		if rank >= permRank("admin") {
			result[d.ID] = PermFlags{CanEdit: true, CanDelete: isOrgAdmin, CanShare: true}
		} else {
			needCollabs = append(needCollabs, d.ID)
			result[d.ID] = PermFlags{CanEdit: rank >= permRank("editor"), CanDelete: isOrgAdmin, CanShare: false}
		}
	}

	if len(needCollabs) > 0 {
		collabMap, err := a.storage.ListCollaboratorsBatch(ctx, needCollabs)
		if err == nil {
			for _, flowID := range needCollabs {
				collabs := collabMap[flowID]
				rank := 0
				for _, c := range collabs {
					if c.UserID == userID {
						if r := permRank(c.Permission); r > rank {
							rank = r
						}
					}
				}
				prev := result[flowID]
				if rank >= permRank("editor") {
					prev.CanEdit = true
				}
				if rank >= permRank("admin") {
					prev.CanShare = true
				}
				result[flowID] = prev
			}
		}
	}

	return result
}

// RequireOrgMember returns nil if userID is a member of orgID (any role),
// ErrPermissionDenied otherwise.
func (a *AuthzService) RequireOrgMember(ctx context.Context, orgID, userID string) error {
	if a.storage == nil { // Local mode
		return nil
	}
	if a.orgSvc != nil && a.orgSvc.IsMember(ctx, orgID, userID) {
		return nil
	}
	return ErrPermissionDenied
}

// RequireOrgAdmin returns nil if userID is an admin of orgID.
func (a *AuthzService) RequireOrgAdmin(ctx context.Context, orgID, userID string) error {
	if a.storage == nil { // Local mode
		return nil
	}
	if a.orgSvc != nil && a.orgSvc.IsAdmin(ctx, orgID, userID) {
		return nil
	}
	return ErrPermissionDenied
}

// RequireOrgWriter returns nil if userID's role in orgID ranks at least
// "member" (i.e. may create content in the org; viewers/guests may not).
func (a *AuthzService) RequireOrgWriter(ctx context.Context, orgID, userID string) error {
	if a.storage == nil { // Local mode
		return nil
	}
	if a.orgSvc != nil {
		if role, err := a.orgSvc.MemberRole(ctx, orgID, userID); err == nil {
			if orgRoleToPermRank(role) >= permRank("editor") {
				return nil
			}
		}
	}
	return ErrPermissionDenied
}

func permRank(p string) int {
	switch p {
	case "admin":
		return 30
	case "editor":
		return 20
	case "viewer":
		return 10
	default:
		return 0
	}
}

func orgRoleToPermRank(role auth.Role) int {
	switch role {
	case auth.RoleAdmin:
		return 30
	case auth.RoleMember:
		return 20
	default:
		return 10
	}
}
