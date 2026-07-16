package models

import "maps"

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

type GeneralSettings struct {
	FirstRunCompleted bool   `json:"firstRunCompleted"`
	LastUsedVersion   string `json:"lastUsedVersion"`
	CheckForUpdates   string `json:"checkForUpdates"`
	OpenInNewWindow   bool   `json:"openInNewWindow"`
}

type AppearanceSettings struct {
	Theme        ThemeMode `json:"theme"`
	Density      string    `json:"density"`
	CodeFont     string    `json:"codeFont"`
	UIFont       string    `json:"uiFont"`
	ReduceMotion bool      `json:"reduceMotion"`
	HighContrast bool      `json:"highContrast"`
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
	ActiveProvider          string                      `json:"activeProvider"`
	EmbeddingProvider       string                      `json:"embeddingProvider"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	DemoMode                DemoModeSettings            `json:"demoMode"`
	ShowCostEstimates       bool                        `json:"showCostEstimates"`
	SaveConversationHistory bool                        `json:"saveConversationHistory"`
	SystemPromptSuffix      string                      `json:"systemPromptSuffix,omitempty"`
	DailyBudget             float64                     `json:"dailyBudget"`
	Prompts                 AIPromptsConfig             `json:"prompts"`
}

type AIProviderConfig struct {
	Enabled            bool    `json:"enabled"`
	DefaultModel       string  `json:"defaultModel"`
	Temperature        float64 `json:"temperature"`
	MaxTokens          int     `json:"maxTokens"`
	ContextTokenBudget int     `json:"contextTokenBudget"`
}

type DemoModeSettings struct {
	Enabled    bool   `json:"enabled"`
	DailyLimit int    `json:"dailyLimit"`
	DailyUsed  int    `json:"dailyUsed"`
	ResetDate  string `json:"resetDate"`
}

type ParserSettings struct {
	MaxFileSizeMB     int  `json:"maxFileSizeMB"`
	PreserveComments  bool `json:"preserveComments"`
	TreatTabsAsSpaces bool `json:"treatTabsAsSpaces"`
	SpacesPerIndent   int  `json:"spacesPerIndent"`
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

// Clone returns a copy safe for a caller to read (and mutate) without affecting
// the original. It copies the reference-typed fields (slice + maps) explicitly;
// all other fields are value types carried by the struct copy. This replaces an
// earlier json.Marshal/Unmarshal round-trip used on hot read paths.
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
		General: GeneralSettings{
			CheckForUpdates: "weekly",
		},
		Appearance: AppearanceSettings{
			Theme:    ThemeDark,
			Density:  "comfortable",
			CodeFont: "JetBrains Mono",
			UIFont:   "Inter",
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
					DefaultModel:       "gemini-2-5-pro",
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
			},
			DemoMode: DemoModeSettings{
				Enabled:    true,
				DailyLimit: 5,
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
			MaxFileSizeMB:     50,
			PreserveComments:  true,
			TreatTabsAsSpaces: true,
			SpacesPerIndent:   4,
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
