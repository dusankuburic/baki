package service

import (
	"context"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// FlowScopeResolver resolves what a library scope covers for a caller — owned, collaborator-shared and every
// org they belong to — as a storage flow filter. The library lists and counts through it and the analytics
// dashboard collects its flow set through it, so the two cannot drift. Cloud mode only: without storage there
// is no library.
type FlowScopeResolver struct {
	storage storageif.StorageBackend
	authz   *AuthzService
}

// NewFlowScopeResolver builds the resolver; storage is nil on the desktop.
func NewFlowScopeResolver(storage storageif.StorageBackend, authz *AuthzService) *FlowScopeResolver {
	return &FlowScopeResolver{storage: storage, authz: authz}
}

// filter centralises scope/org resolution so List and Count cannot
// drift; scope=all without orgID also returns the caller's folded membership list.
func (r *FlowScopeResolver) filter(ctx context.Context, userID, orgID string, scope LibraryScope, query string, sort storageif.FlowSort, limit, offset int) (storageif.FlowFilter, []storageif.OrgMembership, error) {
	filter := storageif.FlowFilter{
		UserID:       userID,
		Query:        query,
		Limit:        limit,
		Offset:       offset,
		SortBy:       sort,
		MetadataOnly: true,
	}
	if orgID != "" {
		if err := r.authz.Can(ctx, auth.ActorFromContext(ctx), OrgRead, OrgByID(orgID)); err != nil {
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
		ms, err := r.memberOrgs(ctx, userID)
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

// memberOrgs lists the orgs userID belongs to with the role in each, without
// rosters. Never nil — empty means "no orgs".
func (r *FlowScopeResolver) memberOrgs(ctx context.Context, userID string) ([]storageif.OrgMembership, error) {
	ms, err := r.storage.ListOrgMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if ms == nil {
		ms = []storageif.OrgMembership{}
	}
	return ms, nil
}

// ScopeFlowIDs resolves the flow ids the caller may see under a scope, paged
// in 500s up to limit; result two lists the widening org ids (org-keyed analytics).
func (r *FlowScopeResolver) ScopeFlowIDs(ctx context.Context, userID, orgID string, scope LibraryScope, limit int) (ids []string, orgIDs []string, err error) {
	defer logger.Guard("LibraryService.ScopeFlowIDs", &err) // the label predates the move out of LibraryService
	ids, orgIDs = []string{}, []string{}
	if r.storage == nil {
		return ids, orgIDs, nil
	}
	if limit <= 0 {
		limit = 5000
	}
	const page = 500
	// The scope (membership check, org list) is resolved ONCE for the walk;
	// it used to be rebuilt — membership queries included — for every page.
	filter, _, ferr := r.filter(ctx, userID, orgID, scope, "", storageif.FlowSortIDAsc, page, 0)
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
		docs, lerr := r.storage.ListFlows(ctx, filter)
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
