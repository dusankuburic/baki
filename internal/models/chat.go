package models

import "time"

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
}

type ChatRequest struct {
	FlowID             string        `json:"flowId"`
	Provider           string        `json:"provider"`
	Model              string        `json:"model,omitempty"`
	Messages           []ChatMessage `json:"messages"`
	UserMessage        string        `json:"userMessage"`
	ContextBlockID     string        `json:"contextBlockId,omitempty"`
	SelectedSourceFiles []string      `json:"selectedSourceFiles,omitempty"`
	SystemPrompt       string        `json:"systemPrompt,omitempty"`
	Temperature        float64       `json:"temperature,omitempty"`
	MaxTokens          int           `json:"maxTokens,omitempty"`
	DemoMode           bool          `json:"demoMode,omitempty"`
	// UseTools opts this request into the read-only tool/agent loop. It only
	// takes effect when the resolved provider supports tools; otherwise the
	// normal streaming path runs.
	UseTools      bool `json:"useTools,omitempty"`
	// ExcludeContext skips injecting flow/block context into the prompt so the
	// user can ask a free-form question without the document being attached.
	ExcludeContext bool `json:"excludeContext,omitempty"`
}

type SourceFileInfo struct {
	Filename   string `json:"filename"`
	SubflowID  string `json:"subflowId"`
	SubflowName string `json:"subflowName"`
	BlockCount int    `json:"blockCount"`
	LineCount  int    `json:"lineCount"`
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
	Version  int           `json:"version"`
	FlowKey  string        `json:"flowKey"`
	Scope    string        `json:"scope"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Messages []ChatMessage `json:"messages"`
}
