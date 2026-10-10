package models

import (
	"maps"
	"slices"
)

type ThemeMode string

const (
	ThemeDark            ThemeMode = "dark"
	ThemeLight           ThemeMode = "light"
	ThemeSystem          ThemeMode = "system"
	ThemeMidnight        ThemeMode = "midnight"
	ThemeWarm            ThemeMode = "warm"
	ThemeTokyoNight      ThemeMode = "tokyo-night"
	ThemeOneDark         ThemeMode = "one-dark"
	ThemeDracula         ThemeMode = "dracula"
	ThemeNord            ThemeMode = "nord"
	ThemeGruvboxDark     ThemeMode = "gruvbox-dark"
	ThemeGruvboxLight    ThemeMode = "gruvbox-light"
	ThemeCatppuccinMocha ThemeMode = "catppuccin-mocha"
	ThemeCatppuccinLatte ThemeMode = "catppuccin-latte"
	ThemeRosePine        ThemeMode = "rose-pine"
	ThemeRosePineMoon    ThemeMode = "rose-pine-moon"
	ThemeRosePineDawn    ThemeMode = "rose-pine-dawn"
	ThemeGithubDark      ThemeMode = "github-dark"
	ThemeGithubLight     ThemeMode = "github-light"
	ThemeKanagawa        ThemeMode = "kanagawa"
	ThemeEverforest      ThemeMode = "everforest"
)

type AppSettings struct {
	Version     int                `json:"version"`
	General     GeneralSettings    `json:"general"`
	Appearance  AppearanceSettings `json:"appearance"`
	Layout      LayoutSettings     `json:"layout"`
	AI          AISettings         `json:"ai"`
	Parser      ParserSettings     `json:"parser"`
	Analysis    AnalysisSettings   `json:"analysis"`
	RecentFiles []RecentFile       `json:"recentFiles"`
}

// GeneralSettings used to also carry lastUsedVersion, checkForUpdates and
// openInNewWindow: persisted and defaulted, but with no control and no reader
// (there is no updater). Stored JSON that still has them decodes fine.
type GeneralSettings struct {
	FirstRunCompleted bool `json:"firstRunCompleted"`
}

type AppearanceSettings struct {
	Theme        ThemeMode `json:"theme"`
	Density      string    `json:"density"`
	ReduceMotion bool      `json:"reduceMotion"`
	HighContrast bool      `json:"highContrast"`
	// ChatTextSize is sm | md | lg for the AI chat's messages.
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
	// EmbeddingModel overrides the provider's default embedding model; empty
	// keeps the default. The pgvector index is dimensionless, so a model swap
	// needs no re-index (mismatched dims are excluded at insert).
	EmbeddingModel          string                      `json:"embeddingModel,omitempty"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	ShowCostEstimates       bool                        `json:"showCostEstimates"`
	SaveConversationHistory bool                        `json:"saveConversationHistory"`
	SystemPromptSuffix      string                      `json:"systemPromptSuffix,omitempty"`
	DailyBudget             float64                     `json:"dailyBudget"`
	// PerUserDailyBudget caps an individual user's AI spend within an org, so
	// one user cannot exhaust the org's DailyBudget alone; 0 = org budget only.
	PerUserDailyBudget float64 `json:"perUserDailyBudget,omitempty"`
	// AllowedProviders, in an ORGANISATION's settings, lists the provider ids its flows and corpus may be
	// sent to (ADR 0028); empty = every provider the deployment offers. Unread in the deployment's and a
	// user's settings.
	AllowedProviders []string        `json:"allowedProviders,omitempty"`
	Prompts          AIPromptsConfig `json:"prompts"`
}

type AIProviderConfig struct {
	Enabled      bool    `json:"enabled"`
	DefaultModel string  `json:"defaultModel"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"maxTokens"`
	// ReasoningEffort is the per-provider reasoning-depth choice ("", "low",
	// "medium" or "high"; "" = provider default), merged onto ChatRequest next
	// to Temperature/MaxTokens; per-turn validation refuses other values.
	ReasoningEffort    string `json:"reasoningEffort,omitempty"`
	ContextTokenBudget int    `json:"contextTokenBudget"`
}

// ParserSettings: only the size cap is read by the parser; the "Preserve
// comments", "Treat tabs as spaces" and "Spaces per indent" controls were
// persisted but never consulted, so they were removed rather than left inert.
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

// Clone returns a copy safe for a caller to read and mutate without
// affecting the original: reference-typed fields (slice + maps) are copied
// explicitly. Replaces a json.Marshal/Unmarshal round-trip on hot read paths.
func (s *AppSettings) Clone() *AppSettings {
	if s == nil {
		return nil
	}
	cp := *s

	if s.RecentFiles != nil {
		cp.RecentFiles = make([]RecentFile, len(s.RecentFiles))
		copy(cp.RecentFiles, s.RecentFiles)
	}
	if s.AI.Providers != nil {
		cp.AI.Providers = maps.Clone(s.AI.Providers)
	}
	if s.AI.AllowedProviders != nil {
		cp.AI.AllowedProviders = slices.Clone(s.AI.AllowedProviders)
	}
	if s.Analysis.Rules != nil {
		cp.Analysis.Rules = make(map[string]RuleConfig, len(s.Analysis.Rules))
		for k, rc := range s.Analysis.Rules {
			if rc.Options != nil {
				rc.Options = maps.Clone(rc.Options)
			}
			cp.Analysis.Rules[k] = rc
		}
	}
	return &cp
}

func DefaultSettings() *AppSettings {
	return &AppSettings{
		Version: 1,
		Appearance: AppearanceSettings{
			Theme:        ThemeDark,
			Density:      "comfortable",
			ChatTextSize: "md",
		},
		Layout: LayoutSettings{
			SidebarWidth:           280,
			InspectorWidth:         320,
			LastActiveInspectorTab: "details",
			LastViewMode:           "block",
		},
		AI: AISettings{
			ActiveProvider:    "claude",
			EmbeddingProvider: "openai",
			Providers: map[string]AIProviderConfig{
				"claude": {
					Enabled:            true,
					DefaultModel:       "claude-sonnet-4-6",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"openai": {
					DefaultModel:       "gpt-4o",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"gemini": {
					DefaultModel:       "gemini-2.5-pro",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"xai": {
					DefaultModel:       "grok-3-mini",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"glm": {
					DefaultModel:       "glm-5.2",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"github-models": {
					DefaultModel:       "gpt-4o",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				"copilot": {
					DefaultModel:       "gpt-4o",
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				// No DefaultModel: Azure AI Foundry models are the user's own
				// deployments, so the server-reported deployment is the default.
				"azure-foundry": {
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
				// No DefaultModel: the served model ids come from the user's
				// own local server (/v1/models).
				"local": {
					Temperature:        0.3,
					MaxTokens:          4096,
					ContextTokenBudget: 4000,
				},
			},
			ShowCostEstimates:       true,
			SaveConversationHistory: true,
			DailyBudget:             5.0, // Default $5/day
			Prompts: AIPromptsConfig{
				Block: []string{
					"Explain this block",
					"Find issues here",
					"Suggest improvements",
					"What does this block do?",
					"Could this block cause errors?",
				},
				Flow: []string{
					"Analyze the whole flow",
					"Find performance issues",
					"Security audit",
					"Find potential bugs",
					"Summarize what this flow does",
				},
				Finding: []string{
					"How do I fix this issue?",
					"Is this a false positive?",
					"Show me similar patterns in the flow",
				},
				BlockWithFindings: []string{
					"How do I fix the issues on this block?",
					"Are these findings related?",
					"Is this a false positive?",
					"Show me similar patterns in the flow",
					"Explain what this block does",
				},
			},
		},
		Parser: ParserSettings{
			MaxFileSizeMB: 50,
		},
		Analysis: AnalysisSettings{
			AutoAnalyzeOnOpen: true,
			Rules: map[string]RuleConfig{
				"unhandled-error":             {Enabled: true, Severity: "warning"},
				"infinite-loop-risk":          {Enabled: true, Severity: "error"},
				"deep-nesting":                {Enabled: true, Severity: "info", Options: map[string]interface{}{"maxDepth": 6}},
				"hardcoded-credential":        {Enabled: true, Severity: "error"},
				"dead-code":                   {Enabled: true, Severity: "info"},
				"missing-delay":               {Enabled: true, Severity: "info"},
				"duplicate-action":            {Enabled: true, Severity: "info", Options: map[string]interface{}{"minRepeats": 3}},
				"unused-variable":             {Enabled: true, Severity: "info"},
				"slow-pattern":                {Enabled: true, Severity: "warning"},
				"empty-handler":               {Enabled: true, Severity: "warning"},
				"uninitialized-variable":      {Enabled: true, Severity: "warning"},
				"resource-leak":               {Enabled: true, Severity: "warning"},
				"subflow-no-error-handler":    {Enabled: true, Severity: "info"},
				"goto-antipattern":            {Enabled: true, Severity: "warning"},
				"empty-branch":                {Enabled: true, Severity: "info"},
				"redundant-action":            {Enabled: true, Severity: "info"},
				"file-op-no-error-handler":    {Enabled: true, Severity: "warning"},
				"missing-timeout":             {Enabled: true, Severity: "warning"},
				"sensitive-exposure":          {Enabled: true, Severity: "error"},
				"error-swallow":               {Enabled: true, Severity: "warning"},
				"missing-retry":               {Enabled: true, Severity: "info"},
				"wide-loop":                   {Enabled: true, Severity: "info", Options: map[string]interface{}{"maxBlocks": 20}},
				"subflow-mismatch":            {Enabled: true, Severity: "warning"},
				"dead-data":                   {Enabled: true, Severity: "info"},
				"hardcoded-filepath":          {Enabled: true, Severity: "info"},
				"sql-injection-risk":          {Enabled: true, Severity: "warning"},
				"hardcoded-ui-coordinates":    {Enabled: true, Severity: "warning"},
				"command-injection-risk":      {Enabled: true, Severity: "error"},
				"insecure-http-url":           {Enabled: true, Severity: "warning"},
				"path-traversal-risk":         {Enabled: true, Severity: "warning"},
				"tainted-sink":                {Enabled: true, Severity: "error"},
				"hardcoded-url":               {Enabled: true, Severity: "info"},
				"large-subflow":               {Enabled: true, Severity: "info", Options: map[string]interface{}{"maxBlocks": 50}},
				"disabled-block":              {Enabled: true, Severity: "info"},
				"hardcoded-ip":                {Enabled: true, Severity: "info"},
				"circular-subflow-dependency": {Enabled: true, Severity: "error"},
				"parse-error":                 {Enabled: true, Severity: "error"},
				"high-cyclomatic-complexity":  {Enabled: true, Severity: "warning"},
				"uncalled-subflow":            {Enabled: true, Severity: "warning"},
				"duplicate-subflow-name":      {Enabled: true, Severity: "error"},
				"duplicate-label":             {Enabled: true, Severity: "warning"},
				"switch-no-default":           {Enabled: true, Severity: "warning"},
				"empty-subflow":               {Enabled: true, Severity: "info"},
				"todo-in-comment":             {Enabled: true, Severity: "info"},
				"wait-zero":                   {Enabled: true, Severity: "info"},
			},
		},
	}
}

type AppInfo struct {
	Version      string          `json:"version"`
	Platform     string          `json:"platform"`
	Arch         string          `json:"arch"`
	BuildDate    string          `json:"buildDate"`
	GitCommit    string          `json:"gitCommit"`
	Capabilities AppCapabilities `json:"capabilities"`
}

type AppCapabilities struct {
	SessionAnalytics bool `json:"sessionAnalytics"`
}

type FrontendError struct {
	Message        string `json:"message"`
	Stack          string `json:"stack"`
	ComponentStack string `json:"componentStack"`
	URL            string `json:"url"`
}
