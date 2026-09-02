package models

import "time"

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
	FinishReason     string    `json:"finishReason,omitempty"`
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
	DemoMode            bool          `json:"demoMode,omitempty"`
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
}

type ProviderInfo struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Configured   bool          `json:"configured"`
	Models       []ModelDetail `json:"models"`
	DefaultModel string        `json:"defaultModel"`
	ContextLimit int           `json:"contextLimit"`
	AuthType     string        `json:"authType"`
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
