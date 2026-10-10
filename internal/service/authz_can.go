package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ErrScopeDenied: the actor is a scoped API token holding none of the scopes the action accepts. It wraps
// ErrPermissionDenied, so render.Error answers 403 and every errors.Is(err, ErrPermissionDenied) check treats it as
// the denial it is; errors.Is(err, ErrScopeDenied) tells it apart from a role refusal.
var ErrScopeDenied = fmt.Errorf("%w: missing API token scope", ErrPermissionDenied)

// errUnknownOrg is OrgAdmin's answer for an organisation that does not exist: the admin routes have always
// answered 404 there. The member actions answer ErrPermissionDenied instead, so they reveal nothing.
var errUnknownOrg = fmt.Errorf("%w: no such organisation", ErrNotFound)

// Action is what an actor does to a resource. Each action takes a relationship to the resource and accepts some
// API token scopes; actionSpecs is the table.
type Action uint8

const (
	// FlowRead: read a flow and what hangs off it (analysis, findings, triage, comments, versions, exports,
	// collaborators). Viewer access; scope read or write.
	FlowRead Action = iota + 1
	// FlowEdit: change a flow or its records (blocks, source, fixes, tags, triage, baselines, comments, share
	// links), or preview such a change. Editor access; scope write.
	FlowEdit
	// FlowShare: manage who has access to a flow (collaborators) and moderate its comments. Admin access; scope
	// write.
	FlowShare
	// FlowDelete: delete a flow. The owner or an admin of its organisation, never a collaborator; scope write.
	FlowDelete
	// ChatUse: chat about a flow (stream, read the conversation, preview the context). Viewer access; scope chat.
	ChatUse
	// OrgRead: read an organisation (roster, channels, rules, settings, knowledge, policies). Any member; scope
	// read or write.
	OrgRead
	// OrgUse: see and run on what an organisation shares (its AI resources and credentials,
	// OrgRoleUsesSharedCredential). Admins and members; scope read or write.
	OrgUse
	// OrgWrite: create content in an organisation (library flows). Admins and members; scope write.
	OrgWrite
	// OrgAdmin: manage an organisation (the organisation itself, members, invites, channels, rules, knowledge, AI
	// resources, settings, policies). Admins; scope write.
	OrgAdmin
	// InstanceAdmin: administer the deployment (/api/admin, the deployment-wide rule settings). The global admin
	// role; scope admin.
	InstanceAdmin
	// InstanceWrite: create content at all (upload or save a flow, test a custom rule, save a conversation). The
	// global admin or member role: global viewers and guests are read-only. Scope write.
	InstanceWrite
	// CIRun: run the CI webhook on a flow (analyse it and record the run) with a personal API token. Viewer access,
	// as reading the flow's analysis, and on an organisation's flow membership of it: the run uses the organisation's
	// custom rules, which a collaborator outside it does not see, and records onto its dashboard. Scope ci, which
	// nothing else accepts.
	CIRun
)

type actionSpec struct {
	name string
	on   resourceKind
	// scopes are the token scopes that permit the action, any one of them. Write also permits the reads: the
	// read-style POST routes (analyze, export, triage lists) have always admitted write-only tokens.
	scopes []string
	// flowPerm is the CheckFlowAccess permission a flow action needs; FlowDelete is CanDeleteFlow's instead.
	flowPerm string
	// orgMember: on an organisation's flow the action also needs membership of the organisation, whatever grants the
	// flow (a collaborator outside it, an owner who left it).
	orgMember bool
	// orgRank is the orgRoleToPermRank an org action needs.
	orgRank int
	// role is the global role an instance action needs at least.
	role auth.Role
}

var (
	readScopes = []string{auth.ScopeRead, auth.ScopeWrite}
	writeScope = []string{auth.ScopeWrite}
)

var actionSpecs = [...]actionSpec{
	FlowRead:      {name: "flow.read", on: resFlow, scopes: readScopes, flowPerm: "viewer"},
	FlowEdit:      {name: "flow.edit", on: resFlow, scopes: writeScope, flowPerm: "editor"},
	FlowShare:     {name: "flow.share", on: resFlow, scopes: writeScope, flowPerm: "admin"},
	FlowDelete:    {name: "flow.delete", on: resFlow, scopes: writeScope},
	ChatUse:       {name: "chat.use", on: resFlow, scopes: []string{auth.ScopeChat}, flowPerm: "viewer"},
	OrgRead:       {name: "org.read", on: resOrg, scopes: readScopes, orgRank: permRank("viewer")},
	OrgUse:        {name: "org.use", on: resOrg, scopes: readScopes, orgRank: permRank("editor")},
	OrgWrite:      {name: "org.write", on: resOrg, scopes: writeScope, orgRank: permRank("editor")},
	OrgAdmin:      {name: "org.admin", on: resOrg, scopes: writeScope, orgRank: permRank("admin")},
	InstanceAdmin: {name: "instance.admin", on: resInstance, scopes: []string{auth.ScopeAdmin}, role: auth.RoleAdmin},
	InstanceWrite: {name: "instance.write", on: resInstance, scopes: writeScope, role: auth.RoleMember},
	CIRun:         {name: "ci.run", on: resFlow, scopes: []string{auth.ScopeCI}, flowPerm: "viewer", orgMember: true},
}

func (a Action) valid() bool { return a > 0 && int(a) < len(actionSpecs) }

func (a Action) String() string {
	if !a.valid() {
		return fmt.Sprintf("action(%d)", uint8(a))
	}
	return actionSpecs[a].name
}

type resourceKind uint8

const (
	resFlow resourceKind = iota + 1
	resOrg
	resInstance
)

func (k resourceKind) String() string {
	switch k {
	case resFlow:
		return "flow"
	case resOrg:
		return "organisation"
	case resInstance:
		return "instance"
	}
	return "no resource"
}

// Resource is what an action is checked against: a flow, an organisation or the deployment. Build one with
// FlowByID, FlowHeader, OrgByID or Instance; Can refuses the zero Resource.
type Resource struct {
	kind   resourceKind
	id     string
	header *storageif.FlowDocument
}

// FlowByID is the flow with id; Can reads its header.
func FlowByID(id string) Resource { return Resource{kind: resFlow, id: id} }

// FlowHeader is a flow whose header the caller has already loaded from storage, which saves Can the read. Its
// owner and organisation decide, so it must be the stored header, never one built from request input. A nil or
// id-less header is refused.
func FlowHeader(h *storageif.FlowDocument) Resource {
	if h == nil || h.ID == "" {
		return Resource{}
	}
	return Resource{kind: resFlow, id: h.ID, header: h}
}

// OrgByID is the organisation with id.
func OrgByID(id string) Resource { return Resource{kind: resOrg, id: id} }

// Instance is the deployment itself, the resource of the global-role actions.
func Instance() Resource { return Resource{kind: resInstance} }

func (r Resource) String() string {
	if r.id == "" {
		return r.kind.String()
	}
	return r.kind.String() + " " + r.id
}

// CanAttempt is the first stage of Can, which needs no resource and reads nothing: a nil actor, an unknown action
// or an actor without a kind or user is ErrPermissionDenied, a scoped API token holding none of the action's
// scopes ErrScopeDenied, and anyone else may attempt the action, subject to Can's check of the resource. Use it
// to refuse before the resource is known (the chat stream's fix approval), never instead of Can.
func CanAttempt(actor *auth.Actor, action Action) error {
	if actor == nil || !action.valid() {
		return ErrPermissionDenied
	}
	switch actor.Kind {
	case auth.ActorSystem:
		return nil
	case auth.ActorUser, auth.ActorToken:
	default:
		return ErrPermissionDenied
	}
	if actor.UserID == "" {
		return ErrPermissionDenied
	}
	spec := &actionSpecs[action]
	for _, s := range spec.scopes {
		if actor.Allows(s) {
			return nil
		}
	}
	return fmt.Errorf("%w (%s needs %s)", ErrScopeDenied, action, strings.Join(spec.scopes, " or "))
}

// precheck is CanAttempt plus the check that action applies to a resource of kind: everything decided before a read.
func precheck(actor *auth.Actor, action Action, kind resourceKind) error {
	if err := CanAttempt(actor, action); err != nil {
		return err
	}
	if kind == 0 || actionSpecs[action].on != kind {
		return fmt.Errorf("%w: %s does not apply to %s", ErrPermissionDenied, action, kind)
	}
	return nil
}

// Can reports whether actor may perform action on res: nil when allowed, otherwise why not. In order:
//   - CanAttempt, before any read: a nil or invalid actor is ErrPermissionDenied, a scoped API token without the
//     action's scope ErrScopeDenied;
//   - a system actor may do everything;
//   - the actor's role, ownership and membership, decided by CheckFlowAccess, CanDeleteFlow and the org role
//     checks. A missing flow is the store's ErrNotFound and a refusal ErrPermissionDenied, as those checks have
//     always answered (404 and 403). An unknown organisation is ErrNotFound for OrgAdmin only, as the admin routes
//     answer; the other org actions refuse, so they reveal nothing about which organisations exist.
//
// Without storage (the desktop app) the flow and org checks allow any valid actor, as they always have; the
// instance actions still need the global role, which auth.LocalActor carries.
func (a *AuthzService) Can(ctx context.Context, actor *auth.Actor, action Action, res Resource) error {
	if err := precheck(actor, action, res.kind); err != nil {
		return err
	}
	if actor.Kind == auth.ActorSystem {
		return nil
	}
	spec := &actionSpecs[action]
	switch res.kind {
	case resInstance:
		// The global roles rank like the org roles: admin, member, then the read-only viewer and guest.
		if orgRoleToPermRank(actor.Role) < orgRoleToPermRank(spec.role) {
			return ErrPermissionDenied
		}
		return nil
	case resOrg:
		if a.storage == nil { // Local mode
			return nil
		}
		return a.requireOrgRank(ctx, res.id, actor.UserID, spec.orgRank, action == OrgAdmin)
	}
	if a.storage == nil { // Local mode
		return nil
	}
	h := res.header
	if h == nil {
		loaded, err := a.storage.LoadFlowHeader(ctx, res.id)
		if err != nil {
			return err
		}
		h = loaded
	}
	return a.checkFlow(ctx, actor.UserID, action, h)
}

// AuthorizeFlow is Can for the flow with flowID that also returns the header it read, for the caller to reuse (the
// version for a document-cache check, the name for a notification) instead of reading it again. CanAttempt still
// runs before the read; a system actor gets the header unchecked. Cloud mode only: callers handle local mode first.
func (a *AuthzService) AuthorizeFlow(ctx context.Context, actor *auth.Actor, action Action, flowID string) (*storageif.FlowDocument, error) {
	if err := precheck(actor, action, resFlow); err != nil {
		return nil, err
	}
	if a.storage == nil {
		return nil, ErrPermissionDenied
	}
	h, err := a.storage.LoadFlowHeader(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if actor.Kind != auth.ActorSystem {
		if err := a.checkFlow(ctx, actor.UserID, action, h); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// AuthorizeOrg is Can for the organisation with orgID that also returns it, roster included: the org routes that
// render the organisation they check decide on the roster they load, one read instead of two. Errors are Can's.
// Without storage it returns the organisation unchecked, as Can allows.
func (a *AuthzService) AuthorizeOrg(ctx context.Context, actor *auth.Actor, action Action, orgID string) (*storageif.Organisation, error) {
	if err := precheck(actor, action, resOrg); err != nil {
		return nil, err
	}
	if a.orgSvc == nil {
		return nil, ErrPermissionDenied
	}
	revealMissing := action == OrgAdmin
	org, err := a.orgSvc.Get(ctx, orgID)
	if err != nil {
		return nil, orgLookupError(err, revealMissing)
	}
	if actor.Kind == auth.ActorSystem || a.storage == nil {
		return org, nil
	}
	for _, m := range org.Members {
		if m.UserID == actor.UserID {
			if orgRoleToPermRank(m.Role) < actionSpecs[action].orgRank {
				return nil, ErrPermissionDenied
			}
			return org, nil
		}
	}
	return nil, ErrPermissionDenied
}

// checkFlow decides a flow action for userID on the stored header h.
func (a *AuthzService) checkFlow(ctx context.Context, userID string, action Action, h *storageif.FlowDocument) error {
	if action == FlowDelete {
		if a.CanDeleteFlow(ctx, h.OwnerID, h.OrganizationID, userID) {
			return nil
		}
		return ErrPermissionDenied
	}
	if err := a.CheckFlowAccess(ctx, h.ID, h.OwnerID, h.OrganizationID, userID, actionSpecs[action].flowPerm); err != nil {
		return err
	}
	if actionSpecs[action].orgMember && h.OrganizationID != "" {
		// Any role: membership is what shows a reader the organisation's custom rules (RuleProfile.Narrowed).
		return a.requireOrgRank(ctx, h.OrganizationID, userID, permRank("viewer"), false)
	}
	return nil
}

// requireOrgRank checks that userID's role in orgID ranks at least need (orgRoleToPermRank), through the request's
// memo. Every failure is ErrPermissionDenied unless revealMissing, which keeps errUnknownOrg for an unknown
// organisation and a failed read's own error. Cloud mode only.
func (a *AuthzService) requireOrgRank(ctx context.Context, orgID, userID string, need int, revealMissing bool) error {
	if a.orgSvc == nil {
		return ErrPermissionDenied
	}
	role, err := a.memberRole(ctx, orgID, userID)
	if err != nil {
		return orgLookupError(err, revealMissing)
	}
	if orgRoleToPermRank(role) < need {
		return ErrPermissionDenied
	}
	return nil
}

// orgLookupError is the answer for a failed membership or organisation read: ErrPermissionDenied for a
// non-member, and for anything else unless revealMissing, which keeps errUnknownOrg for an unknown organisation
// and a failed read's own error.
func orgLookupError(err error, revealMissing bool) error {
	switch {
	case !revealMissing || errors.Is(err, collaboration.ErrMemberNotFound):
		return ErrPermissionDenied
	case errors.Is(err, storageif.ErrNotFound) || errors.Is(err, collaboration.ErrOrgNotFound):
		return errUnknownOrg
	}
	return err
}
