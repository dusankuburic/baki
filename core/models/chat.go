package models

import (
	"encoding/json"
	"time"
)

// ToolCallRecord is one tool invocation behind an assistant message — the
// transparency trail, persisted with the conversation so it survives reloads.
type ToolCallRecord struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Ok         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// FixItemSnapshot is one fix inside a batch approval record, with its own resolved outcome.
type FixItemSnapshot struct {
	RuleID     string `json:"ruleId"`
	FixType    string `json:"fixType"`
	BlockLabel string `json:"blockLabel"`
	Line       int    `json:"line"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// FixProposalSnapshot persists an apply_fix approval prompt with its resolved
// outcome on the assistant message (outliving the stream); Items carries
// per-fix outcomes for batch approvals (empty for single-fix records).
type FixProposalSnapshot struct {
	ProposalID string            `json:"proposalId"`
	RuleID     string            `json:"ruleId"`
	FixType    string            `json:"fixType"`
	BlockLabel string            `json:"blockLabel"`
	Line       int               `json:"line"`
	Summary    string            `json:"summary"`
	Status     string            `json:"status"`
	Message    string            `json:"message,omitempty"`
	Items      []FixItemSnapshot `json:"items,omitempty"`
}

type ChatMessage struct {
	ID               string    `json:"id"`
	Role             string    `json:"role"`
	Content          string    `json:"content"`
	Timestamp        time.Time `json:"timestamp"`
	ContextBlockID   string    `json:"contextBlockId,omitempty"`
	ContextSubflowID string    `json:"contextSubflowId,omitempty"`
	TokensIn         int       `json:"tokensIn,omitempty"`
	TokensOut        int       `json:"tokensOut,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	Model            string    `json:"model,omitempty"`
	// ModelLabel is the model's friendly name at send time (Model is the opaque
	// "m/<entryId>"), so history and exports keep naming it after renames;
	// empty for catalogue models.
	ModelLabel string `json:"modelLabel,omitempty"`
	// ServedModel is the model that actually answered when Model is a resolvable
	// alias, and Input/OutputCostPerM the USD per 1M rates billed — all from the
	// stream's done event, so a reloaded conversation prices the turn at what it
	// cost. Zero/empty = not reported.
	ServedModel    string  `json:"servedModel,omitempty"`
	InputCostPerM  float64 `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64 `json:"outputCostPerM,omitempty"`
	FinishReason   string  `json:"finishReason,omitempty"`
	// ToolCalls records the tool invocations behind this assistant message.
	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`
	// FixProposal snapshots the single apply_fix approval record — LEGACY,
	// superseded by FixProposals; kept for pre-migration conversations.
	FixProposal *FixProposalSnapshot `json:"fixProposal,omitempty"`
	// FixProposals snapshots every apply_fix/apply_fixes approval record: a
	// stream can carry several sequential proposals, a batch several fixes.
	FixProposals []FixProposalSnapshot `json:"fixProposals,omitempty"`
}

type ChatRequest struct {
	FlowID              string        `json:"flowId"`
	Provider            string        `json:"provider"`
	Model               string        `json:"model,omitempty"`
	Messages            []ChatMessage `json:"messages"`
	UserMessage         string        `json:"userMessage"`
	ContextBlockID      string        `json:"contextBlockId,omitempty"`
	SelectedSourceFiles []string      `json:"selectedSourceFiles,omitempty"`
	SystemPrompt        string        `json:"systemPrompt,omitempty"`
	Temperature         float64       `json:"temperature,omitempty"`
	MaxTokens           int           `json:"maxTokens,omitempty"`
	// ReasoningEffort is the per-provider reasoning depth ("", "low", "medium",
	// "high"; anything else is refused), merged from the persisted setting.
	// Only reasoning models use it; the wire repair path drops it when rejected.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// UseTools opts into the read-only tool/agent loop; only effective when the provider supports tools.
	UseTools bool `json:"useTools,omitempty"`
	// ExcludeContext skips flow/block context injection for free-form questions.
	ExcludeContext bool `json:"excludeContext,omitempty"`
	// ClientStreamID is a client UUID used as the stream identifier, letting
	// the client subscribe its SSE listener BEFORE creating the stream (no
	// /chat/begin round-trip); collisions are rejected. Absent ⇒ the backend
	// generates the ID (legacy two-POST handshake).
	ClientStreamID string `json:"clientStreamId,omitempty"`
	// ConversationID is the client thread and the server-side conversation key
	// (history + persistence); before it existed the key was
	// ContextBlockID-or-"flow" and every flow-scoped thread shared ONE stored
	// conversation. Must be a single safe path/key segment (UUID).
	ConversationID string `json:"conversationId,omitempty"`
	// UserMessageID / AssistantMessageID are the client's UUIDs for this turn's
	// two messages; the server persists its own copies under the same ids so a
	// reloaded conversation dedupes by id. Non-UUIDs are ignored.
	UserMessageID      string `json:"userMessageId,omitempty"`
	AssistantMessageID string `json:"assistantMessageId,omitempty"`
	// TemperatureSet reports the request body carried a "temperature" key, so
	// an explicit 0 is honoured instead of read as "provider default"; set by
	// UnmarshalJSON, never on the wire.
	TemperatureSet bool `json:"-"`
}

// UnmarshalJSON decodes a ChatRequest and records whether "temperature" was
// present (TemperatureSet): Temperature alone cannot tell an explicit 0 from
// an omitted field.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type plain ChatRequest
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var probe struct {
		Temperature *json.RawMessage `json:"temperature"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	*r = ChatRequest(p)
	r.TemperatureSet = probe.Temperature != nil && string(*probe.Temperature) != "null"
	return nil
}

// ConversationKey returns the key this conversation is stored and
// reconstructed under: the thread's ConversationID when sent, else the legacy
// ContextBlockID-or-"flow" scope.
func (r ChatRequest) ConversationKey() string {
	if r.ConversationID != "" {
		return r.ConversationID
	}
	if r.ContextBlockID != "" {
		return r.ContextBlockID
	}
	return "flow"
}

type SourceFileInfo struct {
	Filename    string `json:"filename"`
	SubflowID   string `json:"subflowId"`
	SubflowName string `json:"subflowName"`
	BlockCount  int    `json:"blockCount"`
	LineCount   int    `json:"lineCount"`
}

type ChatResponse struct {
	Message       ChatMessage `json:"message"`
	StreamID      string      `json:"streamId,omitempty"`
	EstimatedCost float64     `json:"estimatedCost"`
}

type ModelDetail struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"displayName"`
	ContextLimit   int     `json:"contextLimit"`
	InputCostPerM  float64 `json:"inputCostPerM"`
	OutputCostPerM float64 `json:"outputCostPerM"`
	// SupportsTools reports whether the provider family behind this model
	// supports the agentic tool loop. Provider-level granularity: mixed
	// families (GitHub Models) report true — the runtime degrades per model
	// (ErrToolsUnsupported).
	SupportsTools bool `json:"supportsTools"`

	// The fields below describe an Azure AI Foundry registry entry (ID
	// "m/<entryId>"); catalogue models leave them empty, embedding entries are
	// never listed.
	// Kind is the entry's kind: "chat", "claude" or "agent".
	Kind string `json:"kind,omitempty"`
	// Purpose is a preset key (general, documentation, fixes, review,
	// refactoring, testing) or "custom" with PurposeCustom's text; the picker
	// groups by it.
	Purpose       string `json:"purpose,omitempty"`
	PurposeCustom string `json:"purposeCustom,omitempty"`
	// Description is shown under the name in the picker; admin-written, plain text only.
	Description string `json:"description,omitempty"`
	// Shared marks an organisation's entry (not the caller's own); OrgID and OrgName name it.
	Shared  bool   `json:"shared,omitempty"`
	OrgID   string `json:"orgId,omitempty"`
	OrgName string `json:"orgName,omitempty"`
	// ConnectionID / ConnectionName name the entry's resource.
	ConnectionID   string `json:"connectionId,omitempty"`
	ConnectionName string `json:"connectionName,omitempty"`
	// Status is "ready" or why the entry cannot be used right now
	// (needs_credentials, unreadable, invalid_config, unsupported);
	// StatusReason carries the copy.
	Status       string `json:"status,omitempty"`
	StatusReason string `json:"statusReason,omitempty"`
	// Aliases are the pre-registry ids the entry answers for ("gpt-4.1", "claude:x", "agent:y").
	Aliases []string `json:"aliases,omitempty"`
	// IsDefault marks the server default for new chats; IsOrgDefault the organisation's default.
	IsDefault    bool `json:"isDefault,omitempty"`
	IsOrgDefault bool `json:"isOrgDefault,omitempty"`
}

type ProviderInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	// Shared is true when the caller has no credential of their own and the listing runs on the organisation's shared one.
	Shared       bool          `json:"shared,omitempty"`
	Models       []ModelDetail `json:"models"`
	DefaultModel string        `json:"defaultModel"`
	ContextLimit int           `json:"contextLimit"`
	AuthType     string        `json:"authType"`
	// SuggestDiscovery (Azure AI Foundry) offers the "your project has models
	// you haven't added" prompt: the caller's migrated resource can list its
	// project and the prompt was not dismissed.
	SuggestDiscovery bool `json:"suggestDiscovery,omitempty"`
}

type ProviderTestResult struct {
	Ok      bool   `json:"ok"`
	Latency int    `json:"latencyMs"`
	Error   string `json:"error,omitempty"`
}

type DeviceAuthResponse struct {
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

type GitHubAuthResult struct {
	Status string `json:"status"`
	Token  string `json:"token,omitempty"`
	Error  string `json:"error,omitempty"`
}

type GitHubUser struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

type ContextPreview struct {
	SystemPrompt    string `json:"systemPrompt"`
	ContextText     string `json:"contextText"`
	UserMessage     string `json:"userMessage"`
	EstimatedTokens int    `json:"estimatedTokens"`
	ContextLimit    int    `json:"contextLimit"`
}

type ConversationFile struct {
	Version   int           `json:"version"`
	FlowKey   string        `json:"flowKey"`
	Scope     string        `json:"scope"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Messages  []ChatMessage `json:"messages"`
}
