package audit

// The audit action strings. Stored rows and the admin filter use the values, so a value never changes.
const (
	ActionLogin           = "auth.login"
	ActionLogout          = "auth.logout"
	ActionRegister        = "auth.register"
	ActionPasswordChange  = "auth.password_change"
	ActionRoleChange      = "admin.role_change"
	ActionFlowUpload      = "flow.upload"
	ActionFlowAnalyze     = "flow.analyze"
	ActionFlowExport      = "flow.export"
	ActionFlowDelete      = "flow.delete"
	ActionFlowShare       = "flow.share"
	ActionFlowSave        = "flow.save"
	ActionFlowVersion     = "flow.version_save"
	ActionFlowRestore     = "flow.version_restore"
	ActionFindingTriage   = "finding.triage"
	ActionBaselineSet     = "flow.baseline_set"
	ActionBaselineClear   = "flow.baseline_clear"
	ActionChatStream      = "chat.stream"
	ActionSettingsChange  = "settings.change"
	ActionKeysSave        = "keys.save"
	ActionOrgInviteCreate = "org.invite_create"
	ActionOrgInviteRevoke = "org.invite_revoke"
	ActionOrgInviteAccept = "org.invite_accept"
	ActionOrgMemberAdd    = "org.member_add"
	ActionOrgMemberRemove = "org.member_remove"
	ActionOrgMemberRole   = "org.member_role"
	ActionProfileUpdate   = "user.profile_update"
	ActionAccountDelete   = "user.account_delete"
	ActionDataExport      = "user.data_export"
	ActionSessionRevoke   = "auth.session_revoke"
	ActionSSOLogin        = "auth.sso_login"
	ActionLoginFailure    = "auth.login_failure"
	ActionAccountLock     = "auth.account_lock"
	ActionTokenCreate     = "token.create"
	ActionTokenRevoke     = "token.revoke"
	ActionMigrationStart  = "admin.migration_start"
	// Added by the feature-completeness audit: these mutations previously left no trail at all
	// (share links and policy rewrites being the highest blast-radius non-admin actions).
	ActionCommentAdd       = "finding.comment_add"
	ActionCommentDelete    = "finding.comment_delete"
	ActionShareLinkCreate  = "flow.share_link_create"
	ActionShareLinkRevoke  = "flow.share_link_revoke"
	ActionPolicySave       = "policy.save"
	ActionPolicyDelete     = "policy.delete"
	ActionKnowledgeUpload  = "org.knowledge_upload"
	ActionKnowledgeDelete  = "org.knowledge_delete"
	ActionKnowledgeReindex = "org.knowledge_reindex"
	// ActionKnowledgeIdentityStamp: the knowledge service proved that the caller's embedding model reproduces some
	// legacy documents' vectors and stamped its identity on their chunks (R-knowledge-3; Meta: identity, documents —
	// the proven document ids, comma-separated — and chunks, the rows stamped). Recorded by the service, not a handler.
	ActionKnowledgeIdentityStamp = "org.knowledge_identity_stamp"
	ActionOrgCreate              = "org.create"
	ActionOrgUpdate              = "org.update"
	ActionOrgDelete              = "org.delete"
	ActionScanNow                = "admin.scan_now"
	ActionAlertDismiss           = "governance.alert_dismiss"
	ActionAlertClear             = "governance.alert_clear"
	// These four were emitted as raw strings before; the values are kept so existing rows stay filterable.
	ActionOrgChannelSave   = "org_channel_save"
	ActionOrgChannelDelete = "org_channel_delete"
	ActionOrgRuleSave      = "org_rule_save"
	ActionOrgRuleDelete    = "org_rule_delete"
	ActionSSOLink          = "auth.sso_link"
	ActionSSOUnlink        = "auth.sso_unlink"
	// ActionProvidersTest: an org admin's billed test of the organisation's shared provider
	// credential (the per-user test of one's own key is not audited).
	ActionProvidersTest = "providers.test"
	// The Azure AI Foundry model registry: a resource or model created/updated/removed — the
	// caller's own (azure_connection / azure_model) or an organisation's (org). Meta never a secret.
	ActionAzureConnectionSave   = "azure.connection_save"
	ActionAzureConnectionDelete = "azure.connection_delete"
	ActionAzureModelSave        = "azure.model_save"
	ActionAzureModelDelete      = "azure.model_delete"
	// Organisation CI credentials (ADR 0019): an org admin minted or revoked one (Meta: keyId, name; never the
	// secret), and a CI runner's webhook call analysed a flow (actor kind "integration", the credential's key id as
	// the token id; Meta: method, format, gate, passed).
	ActionOrgCICredentialCreate = "org.ci_credential_create" // #nosec G101 -- an audit action name, not a credential
	ActionOrgCICredentialRevoke = "org.ci_credential_revoke" // #nosec G101 -- an audit action name, not a credential
	ActionCIWebhookRun          = "ci.webhook_run"
	// AI-made and HTTP auto-fixes (R-security-4): a fix written to a flow (Meta: source http|chat, ruleId, fixType,
	// blockId, unsafe, outcome, and for a batch the applied count; from chat also streamId and proposalId), and the
	// user's decision on a chat approval card (Meta: streamId, proposalId, status approved|declined|timeout|cancelled,
	// excludedCount for a batch).
	ActionFlowFixApply    = "flow.fix_apply"
	ActionChatFixDecision = "chat.fix_decision"
	// ActionChatAgentTools: a chat turn in which the provider ran tools of its own (an Azure AI Foundry agent's
	// hosted web search, file search, code interpreter, MCP, …), outside baki's function-tool approvals; recorded
	// when the turn ends, since the chat.stream row is written on accept, before any tool runs (R-provider-29).
	// Meta: streamId, provider, model, tools (the hosted output item types that ran, sorted, comma-separated) and
	// calls (how many started).
	ActionChatAgentTools = "chat.agent_tools"
)

// AllActions is the closed set of audit action strings the server emits; the admin audit UI builds its filter
// from it, so a new constant must be added here too (actions_test.go enforces).
func AllActions() []string {
	return []string{
		ActionLogin, ActionLogout, ActionRegister, ActionPasswordChange,
		ActionRoleChange, ActionFlowUpload, ActionFlowAnalyze, ActionFlowExport,
		ActionFlowDelete, ActionFlowShare, ActionFlowSave, ActionFlowVersion,
		ActionFlowRestore, ActionFindingTriage, ActionBaselineSet, ActionBaselineClear,
		ActionChatStream, ActionSettingsChange, ActionKeysSave, ActionOrgInviteCreate,
		ActionOrgInviteRevoke, ActionOrgInviteAccept, ActionOrgMemberAdd,
		ActionOrgMemberRemove, ActionOrgMemberRole, ActionProfileUpdate,
		ActionAccountDelete, ActionDataExport, ActionSessionRevoke, ActionSSOLogin,
		ActionLoginFailure, ActionAccountLock, ActionTokenCreate, ActionTokenRevoke,
		ActionMigrationStart, ActionCommentAdd, ActionCommentDelete,
		ActionShareLinkCreate, ActionShareLinkRevoke, ActionPolicySave,
		ActionPolicyDelete, ActionKnowledgeUpload, ActionKnowledgeDelete,
		ActionKnowledgeReindex, ActionKnowledgeIdentityStamp, ActionOrgCreate, ActionOrgUpdate, ActionOrgDelete,
		ActionScanNow, ActionAlertDismiss, ActionAlertClear, ActionOrgChannelSave,
		ActionOrgChannelDelete, ActionOrgRuleSave, ActionOrgRuleDelete,
		ActionSSOLink, ActionSSOUnlink, ActionProvidersTest,
		ActionAzureConnectionSave, ActionAzureConnectionDelete,
		ActionAzureModelSave, ActionAzureModelDelete,
		ActionOrgCICredentialCreate, ActionOrgCICredentialRevoke, ActionCIWebhookRun,
		ActionFlowFixApply, ActionChatFixDecision, ActionChatAgentTools,
	}
}
