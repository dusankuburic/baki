// Package events holds the domain events: snapshots of what changed, published by the service that changed it
// and handled by whoever keeps something derived from it (a cache, a search index, a realtime client). The types
// depend on nothing, so a publisher and its subscribers never import each other. The bus that carries them is
// internal/platform/eventbus; docs/adr/0009-domain-events.md has the rules.
//
// An event names everything a subscriber needs to act without a read of its own (ids, owner, organisation), and
// nothing secret. Fields are only ever added.
package events

// FlowChangeKind says what happened to a flow.
type FlowChangeKind string

const (
	FlowCreated FlowChangeKind = "created"
	FlowUpdated FlowChangeKind = "updated"
	FlowDeleted FlowChangeKind = "deleted"
	// FlowSharingChanged: the flow's collaborators or share links changed; its content did not.
	FlowSharingChanged FlowChangeKind = "sharing"
)

// FlowChanged: a flow's document, metadata or sharing changed.
type FlowChanged struct {
	FlowID   string
	StableID string
	OwnerID  string
	OrgID    string
	Kind     FlowChangeKind
	// Version is the flow's stored version after the change when the publisher knows it, else 0. Desktop flows
	// have none.
	Version int64
}

// CredentialChangeKind says what happened to a credential.
type CredentialChangeKind string

const (
	CredentialSaved   CredentialChangeKind = "saved"
	CredentialDeleted CredentialChangeKind = "deleted"
	// CredentialScopeErased: every credential of the scope is gone (the account behind it was erased).
	CredentialScopeErased CredentialChangeKind = "scope-erased"
)

// CredentialChanged: an AI provider credential or configuration of a key scope changed.
type CredentialChanged struct {
	// Scope is the key scope ("" for the deployment/local scope, a user's scope, or ai.OrgScope(orgID)).
	Scope string
	// ProviderID is the provider whose credential changed; "" means every provider of the scope.
	ProviderID string
	OrgID      string
	Kind       CredentialChangeKind
}

// OrgMemberChangeKind says what happened to a membership.
type OrgMemberChangeKind string

const (
	OrgMemberAdded       OrgMemberChangeKind = "added"
	OrgMemberRemoved     OrgMemberChangeKind = "removed"
	OrgMemberRoleChanged OrgMemberChangeKind = "role"
	// OrgDeleted: the organisation and every membership of it are gone (UserID is "").
	OrgDeleted OrgMemberChangeKind = "org-deleted"
)

// OrgMemberChanged: a membership of an organisation changed.
type OrgMemberChanged struct {
	OrgID  string
	UserID string
	Role   string
	Kind   OrgMemberChangeKind
}

// SettingsScope says whose settings changed.
type SettingsScope string

const (
	DeploymentSettings SettingsScope = "deployment"
	UserSettings       SettingsScope = "user"
	OrgSettings        SettingsScope = "org"
)

// SettingsChanged: settings of a scope changed. ID is the user or organisation; "" for the deployment.
type SettingsChanged struct {
	Scope SettingsScope
	ID    string
}

// KnowledgeDocChangeKind says what happened to a knowledge-base document.
type KnowledgeDocChangeKind string

const (
	KnowledgeDocAdded     KnowledgeDocChangeKind = "added"
	KnowledgeDocDeleted   KnowledgeDocChangeKind = "deleted"
	KnowledgeDocReindexed KnowledgeDocChangeKind = "reindexed"
)

// KnowledgeDocChanged: an organisation's knowledge base changed. DocID is "" when a re-index touched them all.
type KnowledgeDocChanged struct {
	OrgID string
	DocID string
	Kind  KnowledgeDocChangeKind
}

// FindingStatusChanged: the triage state of findings of a flow changed (status, assignee, baseline). FindingKey
// is "" when a batch or a baseline touched several.
type FindingStatusChanged struct {
	FlowID     string
	FindingKey string
	OwnerID    string
	OrgID      string
}

// FindingAssigned: a finding was assigned to someone other than its previous assignee. FlowName is the name the
// assigner's authorization check read (the notification names the flow by it).
type FindingAssigned struct {
	FlowID     string
	FlowName   string
	OwnerID    string
	OrgID      string
	FindingKey string
	RuleID     string
	AssigneeID string
	AssignerID string
}

// FindingCommented: a comment was added to a finding. Body is the comment's text, which the assignee's email
// quotes; FlowName as in FindingAssigned.
type FindingCommented struct {
	FlowID     string
	FlowName   string
	OwnerID    string
	OrgID      string
	FindingKey string
	CommentID  string
	AuthorID   string
	Body       string
}

// AccountErased: a user's account was erased. KeyScope is the credential scope the account's keys lived in.
type AccountErased struct {
	UserID   string
	KeyScope string
}

func (FlowChanged) EventName() string          { return "flow.changed" }
func (CredentialChanged) EventName() string    { return "credential.changed" }
func (OrgMemberChanged) EventName() string     { return "org_member.changed" }
func (SettingsChanged) EventName() string      { return "settings.changed" }
func (KnowledgeDocChanged) EventName() string  { return "knowledge_doc.changed" }
func (FindingStatusChanged) EventName() string { return "finding_status.changed" }
func (AccountErased) EventName() string        { return "account.erased" }
func (FindingAssigned) EventName() string      { return "finding.assigned" }
func (FindingCommented) EventName() string     { return "finding.commented" }
