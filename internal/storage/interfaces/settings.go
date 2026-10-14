package interfaces

import (
	"context"
	"time"
)

// AppSettingsStore covers global/user/org application settings.
type AppSettingsStore interface {
	SaveSettings(ctx context.Context, settings *AppSettings) error
	LoadSettings(ctx context.Context) (*AppSettings, error)
	SaveUserSettings(ctx context.Context, userID string, settings *AppSettings) error
	LoadUserSettings(ctx context.Context, userID string) (*AppSettings, error)
	SaveOrgSettings(ctx context.Context, orgID string, settings *AppSettings) error
	LoadOrgSettings(ctx context.Context, orgID string) (*AppSettings, error)
}

// AppSettings MUST stay in field/tag parity with models.AppSettings — the service bridge round-trips via JSON and silently drops mismatches (service.TestSettingsRoundTrip guards it).
type AppSettings struct {
	Version     int                `json:"version"`
	Profile     ProfileSettings    `json:"profile"`
	General     GeneralSettings    `json:"general"`
	Appearance  AppearanceSettings `json:"appearance"`
	Layout      LayoutSettings     `json:"layout"`
	AI          AISettings         `json:"ai"`
	Parser      ParserSettings     `json:"parser"`
	Analysis    AnalysisSettings   `json:"analysis"`
	RecentFiles []RecentFile       `json:"recentFiles"`
}

// ProfileSettings mirrors models.ProfileSettings — keep tag parity or the round-trip drops it.
type ProfileSettings struct {
	Bio      string `json:"bio,omitempty"`
	JobTitle string `json:"jobTitle,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// Settings types — kept in field/tag parity with internal/models so the toModel/fromModel JSON bridge is lossless.
type GeneralSettings struct {
	FirstRunCompleted bool `json:"firstRunCompleted"`
}

type AppearanceSettings struct {
	Theme        string `json:"theme"`
	Density      string `json:"density"`
	ReduceMotion bool   `json:"reduceMotion"`
	HighContrast bool   `json:"highContrast"`
	ChatTextSize string `json:"chatTextSize,omitempty"`
}

type LayoutSettings struct {
	SidebarWidth           int    `json:"sidebarWidth"`
	InspectorWidth         int    `json:"inspectorWidth"`
	SidebarCollapsed       bool   `json:"sidebarCollapsed"`
	InspectorCollapsed     bool   `json:"inspectorCollapsed"`
	LastActiveInspectorTab string `json:"lastActiveInspectorTab"`
	LastViewMode           string `json:"lastViewMode"`
}

type AIPromptsConfig struct {
	Block             []string `json:"block"`
	Flow              []string `json:"flow"`
	Finding           []string `json:"finding"`
	BlockWithFindings []string `json:"blockWithFindings"`
}

type AISettings struct {
	ActiveProvider    string `json:"activeProvider"`
	EmbeddingProvider string `json:"embeddingProvider"`
	// EmbeddingModel mirrors models.AISettings.EmbeddingModel — keep tag parity or the round-trip drops it.
	EmbeddingModel          string                      `json:"embeddingModel,omitempty"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	ShowCostEstimates       bool                        `json:"showCostEstimates"`
	SaveConversationHistory bool                        `json:"saveConversationHistory"`
	SystemPromptSuffix      string                      `json:"systemPromptSuffix,omitempty"`
	DailyBudget             float64                     `json:"dailyBudget"`
	PerUserDailyBudget      float64                     `json:"perUserDailyBudget,omitempty"`
	// AllowedProviders mirrors models.AISettings.AllowedProviders (an organisation's allowed-provider list).
	AllowedProviders []string        `json:"allowedProviders,omitempty"`
	Prompts          AIPromptsConfig `json:"prompts"`
}

type AIProviderConfig struct {
	Enabled      bool    `json:"enabled"`
	DefaultModel string  `json:"defaultModel"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"maxTokens"`
	// ReasoningEffort mirrors models.AIProviderConfig.ReasoningEffort — keep tag parity or the round-trip drops it.
	ReasoningEffort    string `json:"reasoningEffort,omitempty"`
	ContextTokenBudget int    `json:"contextTokenBudget"`
}

type ParserSettings struct {
	MaxFileSizeMB int `json:"maxFileSizeMB"`
}

type AnalysisSettings struct {
	Rules             map[string]RuleConfig `json:"rules"`
	AutoAnalyzeOnOpen bool                  `json:"autoAnalyzeOnOpen"`
}

type RuleConfig struct {
	Enabled  bool                   `json:"enabled"`
	Severity string                 `json:"severity"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

type RecentFile struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	LastOpen time.Time `json:"lastOpen"`
	IsFolder bool      `json:"isFolder"`
}
