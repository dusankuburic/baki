package models

import (
	"encoding/json"
	"time"
)

// ToolCallRecord is one tool invocation behind an assistant message — the
// transparency trail. It mirrors the wire `tool_result` event and is persisted
// with the conversation so the trail survives reloads.
type ToolCallRecord struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Ok         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// FixItemSnapshot is one fix inside a (possibly batch) approval record, with
// its own resolved outcome.
type FixItemSnapshot struct {
	RuleID     string `json:"ruleId"`
	FixType    string `json:"fixType"`
	BlockLabel string `json:"blockLabel"`
	Line       int    `json:"line"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// FixProposalSnapshot persists an apply_fix approval prompt together with its
// resolved outcome on the assistant message, so the decision record outlives
// the stream (and its transient approval card). Items carries per-fix outcomes
// for batch approvals (empty for single-fix records, whose fields are the
// item).
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
	// ModelLabel is the model's friendly name when the message was sent —
	// an Azure AI Foundry registry entry's display name, whose Model is the
	// opaque "m/<entryId>" — so history, bubbles and exports keep naming the
	// model after it is renamed or removed. Empty for catalogue models.
	ModelLabel string `json:"modelLabel,omitempty"`
	// ServedModel is the model that actually answered when Model is an
	// alias the provider resolves (an Azure AI Foundry deployment,
	// model-router, an agent); InputCostPerM / OutputCostPerM are the USD
	// per 1M token rates the turn was billed at. All three come from the
	// stream's done event and are saved with the message so a reloaded
	// conversation keeps its served-model tag and prices the turn at what it
	// cost, not at the current listing. Zero/empty = not reported.
	ServedModel    string  `json:"servedModel,omitempty"`
	InputCostPerM  float64 `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64 `json:"outputCostPerM,omitempty"`
	FinishReason   string  `json:"finishReason,omitempty"`
	// ToolCalls records the tool invocations behind this assistant message.
	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`
	// FixProposal snapshots the (single) apply_fix approval record, if any.
	// LEGACY field: superseded by FixProposals; kept so conversations saved
	// before the array migration still load.
	FixProposal *FixProposalSnapshot `json:"fixProposal,omitempty"`
	// FixProposals snapshots every apply_fix/apply_fixes approval record on
	// this message — a stream can carry several sequential proposals, and a
	// batch carries several fixes behind one approval.
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
	// ReasoningEffort is the per-provider choice of reasoning depth for
	// reasoning deployments (Settings → AI Behavior): "", "low", "medium" or
	// "high" — anything else is refused. Merged onto the request by the
	// client's request builder from the persisted provider setting; ""
	// keeps the provider default. Only reasoning models use it; the wire
	// repair path drops it again for models that reject the parameter.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// UseTools opts this request into the read-only tool/agent loop. It only
	// takes effect when the resolved provider supports tools; otherwise the
	// normal streaming path runs.
	UseTools bool `json:"useTools,omitempty"`
	// ExcludeContext skips injecting flow/block context into the prompt so the
	// user can ask a free-form question without the document being attached.
	ExcludeContext bool `json:"excludeContext,omitempty"`
	// ClientStreamID, when set, is a client-generated UUID used as the stream
	// identifier. It lets the client subscribe its SSE listener BEFORE creating
	// the stream, so the backend can begin emitting immediately without the
	// extra /chat/begin round-trip (C-1). Must be a UUID; collisions are
	// rejected. When absent the backend generates the ID and the client must
	// send /chat/begin after subscribing (legacy two-POST handshake).
	ClientStreamID string `json:"clientStreamId,omitempty"`
	// ConversationID is the client thread this turn belongs to. It is the
	// server-side conversation key (history reconstruction + persistence).
	// Before it existed the key was ContextBlockID-or-"flow", so every
	// flow-scoped thread on a flow shared ONE stored conversation: thread B was
	// answered with thread A's history and each save clobbered the other.
	// Must be a single safe path/key segment (a UUID); the handler rejects
	// anything else.
	ConversationID string `json:"conversationId,omitempty"`
	// UserMessageID / AssistantMessageID are the client's ids for this turn's
	// two messages (UUIDs). The server persists its own copies of the turn
	// (user turn at stream start, answer at the end) under these same ids, so a
	// client that merges a reloaded conversation with the messages it already
	// holds can dedupe by id instead of showing the turn twice. Anything that is
	// not a UUID is ignored and the server mints its own id.
	UserMessageID      string `json:"userMessageId,omitempty"`
	AssistantMessageID string `json:"assistantMessageId,omitempty"`
	// TemperatureSet reports that the request body carried a "temperature"
	// key, so an explicit 0 (deterministic sampling) is honoured instead of
	// being read as "provider default". Set by UnmarshalJSON; never on the wire.
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

// ConversationKey returns the key under which this turn's conversation is
// stored and reconstructed: the thread's ConversationID when the client sent
// one, otherwise the legacy ContextBlockID-or-"flow" scope (older clients and
// conversations saved before threads had their own keys).
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
	// supports the agentic tool loop (native function calling, or the
	// marker-based fallback). Provider-level granularity: families that mix
	// per-model support (GitHub Models) report true — the runtime degrades
	// gracefully per model (ErrToolsUnsupported).
	SupportsTools bool `json:"supportsTools"`

	// The fields below describe an Azure AI Foundry model registry entry (ID
	// "m/<entryId>"); catalogue models leave them empty. Embedding entries
	// are never listed.
	//
	// Kind is the entry's kind: "chat", "claude" or "agent".
	Kind string `json:"kind,omitempty"`
	// Purpose is a preset key (general, documentation, fixes, review,
	// refactoring, testing) or "custom" with PurposeCustom's text; the
	// picker groups by it.
	Purpose       string `json:"purpose,omitempty"`
	PurposeCustom string `json:"purposeCustom,omitempty"`
	// Description is shown under the name in the picker. Admin-written for
	// shared entries: clients render it as plain text only.
	Description string `json:"description,omitempty"`
	// Shared marks an organisation's entry (not the caller's own); OrgID and
	// OrgName name that organisation.
	Shared  bool   `json:"shared,omitempty"`
	OrgID   string `json:"orgId,omitempty"`
	OrgName string `json:"orgName,omitempty"`
	// ConnectionID / ConnectionName name the entry's resource.
	ConnectionID   string `json:"connectionId,omitempty"`
	ConnectionName string `json:"connectionName,omitempty"`
	// Status is "ready", or why the entry cannot be used right now
	// (needs_credentials, unreadable, invalid_config, unsupported);
	// StatusReason carries the copy.
	Status       string `json:"status,omitempty"`
	StatusReason string `json:"statusReason,omitempty"`
	// Aliases are the pre-registry ids the entry answers for ("gpt-4.1",
	// "claude:x", "agent:y") — only entries of the resource "default".
	Aliases []string `json:"aliases,omitempty"`
	// IsDefault marks the server default for new chats; IsOrgDefault the
	// organisation's "Default for members".
	IsDefault    bool `json:"isDefault,omitempty"`
	IsOrgDefault bool `json:"isOrgDefault,omitempty"`
}

type ProviderInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	// Shared is true when the caller has no credential of their own for the
	// provider and the listing runs on their organisation's shared one.
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
