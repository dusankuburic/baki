package service

import (
	"context"
	"errors"
	"sync"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// AuthzService is the single source of truth for flow- and org-level authorization
// decisions in cloud mode: all services and handlers gating tenant-scoped resources
// must delegate here — do not duplicate policy elsewhere. In local/Tauri mode
// (storage == nil) every check allows: the desktop app is single-user and the API is
// already gated by the static sidecar token.
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

// flowAuthzMemo memoises, for one request, the two store reads every flow
// authorization decision is made of: the caller's org role (OrgService.MemberRole)
// and a flow's collaborator list. A handler making several decisions about one flow
// installs one with WithFlowAuthzMemo; a context without it reads fresh every time.
// Decisions only: a memo must not span a write that changes what it holds. Safe for
// concurrent use.
type flowAuthzMemo struct {
	mu      sync.Mutex
	roles   map[string]memoMemberRole            // orgID + "\x00" + userID
	collabs map[string][]*storageif.Collaborator // flowID
}

// memoMemberRole is one definitive role lookup: the role, or
// collaboration.ErrMemberNotFound. Any other error is not memoised (the next
// decision retries, as it always did).
type memoMemberRole struct {
	role auth.Role
	err  error
}

type flowAuthzMemoKey struct{}

// WithFlowAuthzMemo returns ctx carrying a fresh flow-authorization memo (ctx itself
// when it already carries one).
func WithFlowAuthzMemo(ctx context.Context) context.Context {
	if flowAuthzMemoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, flowAuthzMemoKey{}, &flowAuthzMemo{
		roles:   make(map[string]memoMemberRole),
		collabs: make(map[string][]*storageif.Collaborator),
	})
}

func flowAuthzMemoFrom(ctx context.Context) *flowAuthzMemo {
	m, _ := ctx.Value(flowAuthzMemoKey{}).(*flowAuthzMemo)
	return m
}

// memberRole is orgSvc.MemberRole through the request's memo, when there is
// one. Caller guarantees a.orgSvc != nil.
func (a *AuthzService) memberRole(ctx context.Context, orgID, userID string) (auth.Role, error) {
	memo := flowAuthzMemoFrom(ctx)
	if memo == nil {
		return a.orgSvc.MemberRole(ctx, orgID, userID)
	}
	key := orgID + "\x00" + userID
	memo.mu.Lock()
	r, hit := memo.roles[key]
	memo.mu.Unlock()
	if hit {
		return r.role, r.err
	}
	role, err := a.orgSvc.MemberRole(ctx, orgID, userID)
	if err == nil || errors.Is(err, collaboration.ErrMemberNotFound) {
		memo.mu.Lock()
		memo.roles[key] = memoMemberRole{role: role, err: err}
		memo.mu.Unlock()
	}
	return role, err
}

// collaborators is storage.ListCollaborators (all rows) through the
// request's memo, when there is one. Failed reads are not memoised.
func (a *AuthzService) collaborators(ctx context.Context, flowID string) ([]*storageif.Collaborator, error) {
	memo := flowAuthzMemoFrom(ctx)
	if memo == nil {
		return a.storage.ListCollaborators(ctx, flowID, 0)
	}
	memo.mu.Lock()
	rows, hit := memo.collabs[flowID]
	memo.mu.Unlock()
	if hit {
		return rows, nil
	}
	rows, err := a.storage.ListCollaborators(ctx, flowID, 0)
	if err != nil {
		return nil, err
	}
	memo.mu.Lock()
	memo.collabs[flowID] = rows
	memo.mu.Unlock()
	return rows, nil
}

// isInstanceAdmin reports whether userID holds the platform-level admin role
// (auth.RoleAdmin). Lookup failures deny: authz on the ownerless path fails closed.
func (a *AuthzService) isInstanceAdmin(ctx context.Context, userID string) bool {
	if a.storage == nil || userID == "" {
		return false
	}
	u, err := a.storage.LoadUserByID(ctx, userID)
	if err != nil || u == nil {
		return false
	}
	return u.Role == auth.RoleAdmin
}

// CheckFlowAccess reports whether userID has at least minPerm ("viewer", "editor"
// or "admin") access to the flow identified by flowID with the given owner/org;
// nil when allowed, ErrPermissionDenied otherwise. Access is granted by, in order:
// ownership, org role (admin→admin, member→editor, viewer/guest→viewer), or an
// explicit collaborator grant. Flows with an empty OwnerID are legacy/unclaimed and
// are readable by any authenticated user but never writable.
func (a *AuthzService) CheckFlowAccess(ctx context.Context, flowID, ownerID, orgID, userID, minPerm string) error {
	if a.storage == nil { // Local mode
		return nil
	}

	need := permRank(minPerm)
	if need == 0 {
		// Unrecognised minimum-permission requirement: fail closed rather than
		// treating it as "no requirement" (which would grant any collaborator).
		return ErrPermissionDenied
	}

	// Ownerless legacy flows (pre-dating per-user ownership, or written by a bulk
	// importer without an owner): readable by INSTANCE ADMINS only. They used to be
	// world-readable to every tenant — any registered user could enumerate another
	// org's ingested flow content by UUID. Admins keep triage access; a regular user
	// gets a denial (fail-closed). Assigning an owner restores normal sharing.
	if ownerID == "" {
		if need <= permRank("viewer") && a.isInstanceAdmin(ctx, userID) {
			return nil
		}
		return ErrPermissionDenied
	}

	if ownerID == userID {
		return nil
	}

	// Org role grants tiered access to org flows. A non-member (ErrMemberNotFound)
	// or a transient lookup error must NOT short-circuit the explicit collaborator
	// grant below — org membership is one access path, not a gate; the trailing
	// return fail-closes, mirroring FlowPermissions/BatchFlowPermissions.
	if orgID != "" && a.orgSvc != nil {
		if role, err := a.memberRole(ctx, orgID, userID); err == nil && orgRoleToPermRank(role) >= need {
			return nil
		}
	}

	// Explicit per-flow collaborator grant.
	collabs, err := a.collaborators(ctx, flowID)
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
// CheckFlowAccess, propagating storage errors (notably storageif.ErrNotFound) so
// callers can distinguish 404 from 403.
func (a *AuthzService) CheckFlowAccessByID(ctx context.Context, flowID, userID, minPerm string) error {
	if a.storage == nil { // Local mode
		return nil
	}
	_, err := a.AuthorizeFlowHeader(ctx, flowID, userID, minPerm)
	return err
}

// AuthorizeFlowHeader loads the flow header, checks userID's access against it and,
// when allowed, returns that header for the caller to reuse (the version for a
// document-cache check, the name for a notification) instead of reading it again.
// Header only — authorization needs owner/org, not content or source, and neither is
// transferred or depended on. Storage errors (notably storageif.ErrNotFound)
// propagate so callers can tell 404 from 403. Cloud mode only: callers handle local
// mode (nil storage) first.
func (a *AuthzService) AuthorizeFlowHeader(ctx context.Context, flowID, userID, minPerm string) (*storageif.FlowDocument, error) {
	if a.storage == nil {
		return nil, ErrPermissionDenied
	}
	doc, err := a.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if err := a.CheckFlowAccess(ctx, flowID, doc.OwnerID, doc.OrganizationID, userID, minPerm); err != nil {
		return nil, err
	}
	return doc, nil
}

// CanDeleteFlow reports whether userID may delete the flow. Deletion is deliberately
// narrower than the "admin" permission rank: only the owner or an org admin may
// destroy a flow — an "admin"-tier collaborator can manage sharing but not delete.
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
// against a single flow in one pass — at most one org-role lookup and one
// collaborator lookup, none at all for the owner or an ownerless flow — so list
// endpoints can populate per-row capability flags without a CheckFlowAccess call
// per flag. canDelete mirrors CanDeleteFlow: only the owner or an org admin may
// delete, never an admin-tier collaborator.
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
		if role, err := a.memberRole(ctx, orgID, userID); err == nil {
			if r := orgRoleToPermRank(role); r > rank {
				rank = r
			}
			isOrgAdmin = role == auth.RoleAdmin
		}
	}
	// Only consult collaborators if the org role hasn't already granted top tier.
	if rank < permRank("admin") {
		if collabs, err := a.collaborators(ctx, flowID); err == nil {
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
// 1 + N(distinctOrgs) + 1 queries instead of 3*N, caching org-role lookups per
// distinct org and fetching all collaborators in a single batch query.
func (a *AuthzService) BatchFlowPermissions(ctx context.Context, docs []*storageif.FlowDocument, userID string) map[string]PermFlags {
	return a.BatchFlowPermissionsWithRoles(ctx, docs, userID, nil)
}

// BatchFlowPermissionsWithRoles is BatchFlowPermissions with the caller's org
// memberships already in hand (the library page resolves them to scope its listing,
// so re-querying the role per org was redundant). A non-nil memberships is the
// COMPLETE list: roles are seeded from it, an org absent from it has no role, and no
// per-org query runs at all; nil means "unknown" and roles are looked up per
// distinct org as before.
func (a *AuthzService) BatchFlowPermissionsWithRoles(ctx context.Context, docs []*storageif.FlowDocument, userID string, memberships []storageif.OrgMembership) map[string]PermFlags {
	result := make(map[string]PermFlags, len(docs))
	if a.storage == nil {
		for _, d := range docs {
			result[d.ID] = PermFlags{CanEdit: true, CanDelete: true, CanShare: true}
		}
		return result
	}

	orgRoleCache := make(map[string]auth.Role, len(memberships))
	rolesComplete := memberships != nil
	for _, m := range memberships {
		orgRoleCache[m.OrgID] = m.Role
	}
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
			if !cached && !rolesComplete {
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
