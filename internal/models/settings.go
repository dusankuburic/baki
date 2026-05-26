package models

type ThemeMode string

const (
	ThemeDark       ThemeMode = "dark"
	ThemeLight      ThemeMode = "light"
	ThemeSystem     ThemeMode = "system"
	ThemeMidnight   ThemeMode = "midnight"
	ThemeWarm       ThemeMode = "warm"
	ThemeTokyoNight ThemeMode = "tokyo-night"
	ThemeOneDark    ThemeMode = "one-dark"
	ThemeDracula    ThemeMode = "dracula"
	ThemeNord       ThemeMode = "nord"
)

type AppSettings struct {
	Version    int              `json:"version"`
	General    GeneralSettings  `json:"general"`
	Appearance AppearanceSettings `json:"appearance"`
	Layout     LayoutSettings   `json:"layout"`
	AI         AISettings       `json:"ai"`
	Parser     ParserSettings   `json:"parser"`
	Analysis   AnalysisSettings `json:"analysis"`
	Telemetry  TelemetrySettings `json:"telemetry"`
	RecentFiles []RecentFile    `json:"recentFiles"`
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
	SidebarWidth          int    `json:"sidebarWidth"`
	InspectorWidth        int    `json:"inspectorWidth"`
	SidebarCollapsed      bool   `json:"sidebarCollapsed"`
	InspectorCollapsed    bool   `json:"inspectorCollapsed"`
	LastActiveInspectorTab string `json:"lastActiveInspectorTab"`
	LastViewMode          string `json:"lastViewMode"`
}

type AISettings struct {
	ActiveProvider          string                    `json:"activeProvider"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	DemoMode                DemoModeSettings          `json:"demoMode"`
	ShowCostEstimates       bool                      `json:"showCostEstimates"`
	SaveConversationHistory bool                      `json:"saveConversationHistory"`
	SystemPromptSuffix      string                    `json:"systemPromptSuffix,omitempty"`
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
	MaxFileSizeMB   int  `json:"maxFileSizeMB"`
	PreserveComments bool `json:"preserveComments"`
	TreatTabsAsSpaces bool `json:"treatTabsAsSpaces"`
	SpacesPerIndent  int  `json:"spacesPerIndent"`
}

type AnalysisSettings struct {
	Rules            map[string]RuleConfig `json:"rules"`
	AutoAnalyzeOnOpen bool                 `json:"autoAnalyzeOnOpen"`
}

type RuleConfig struct {
	Enabled   bool              `json:"enabled"`
	Severity  string            `json:"severity"`
	Options   map[string]interface{} `json:"options,omitempty"`
}

type TelemetrySettings struct {
	Enabled       bool   `json:"enabled"`
	AnonymousID   string `json:"anonymousID"`
	LastSubmitted string `json:"lastSubmitted"`
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
			ActiveProvider: "claude",
			Providers: map[string]AIProviderConfig{
				"claude": {
					Enabled:            true,
					DefaultModel:       "claude-sonnet-4-5",
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
		},
		Parser: ParserSettings{
			MaxFileSizeMB:    50,
			PreserveComments: true,
			TreatTabsAsSpaces: true,
			SpacesPerIndent:  4,
		},
		Analysis: AnalysisSettings{
			Rules: map[string]RuleConfig{
				"unhandled-error":      {Enabled: true, Severity: "warning"},
				"infinite-loop-risk":   {Enabled: true, Severity: "error"},
				"deep-nesting":         {Enabled: true, Severity: "info", Options: map[string]interface{}{"maxDepth": 6}},
				"hardcoded-credential": {Enabled: true, Severity: "error"},
				"dead-code":            {Enabled: true, Severity: "info"},
				"missing-delay":        {Enabled: true, Severity: "info"},
				"duplicate-action":     {Enabled: true, Severity: "info", Options: map[string]interface{}{"minRepeats": 3}},
				"unused-variable":         {Enabled: true, Severity: "info"},
				"slow-pattern":            {Enabled: true, Severity: "warning"},
				"empty-handler":           {Enabled: true, Severity: "warning"},
				"uninitialized-variable":  {Enabled: true, Severity: "warning"},
				"resource-leak":           {Enabled: true, Severity: "warning"},
				"subflow-no-error-handler": {Enabled: true, Severity: "info"},
			},
		},
		Telemetry: TelemetrySettings{},
	}
}

type AppInfo struct {
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	BuildDate string `json:"buildDate"`
	GitCommit string `json:"gitCommit"`
}

type FrontendError struct {
	Message        string `json:"message"`
	Stack          string `json:"stack"`
	ComponentStack string `json:"componentStack"`
	URL            string `json:"url"`
}
