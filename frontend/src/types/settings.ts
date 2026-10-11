
import type {KnownProviderID, ProviderID} from './providers'
import type {RuleConfig} from './analysis'

export type ThemeMode =
  | 'dark'
  | 'light'
  | 'system'
  | 'midnight'
  | 'warm'
  | 'tokyo-night'
  | 'one-dark'
  | 'dracula'
  | 'nord'
  | 'gruvbox-dark'
  | 'gruvbox-light'
  | 'catppuccin-mocha'
  | 'catppuccin-latte'
  | 'rose-pine'
  | 'rose-pine-moon'
  | 'rose-pine-dawn'
  | 'github-dark'
  | 'github-light'
  | 'kanagawa'
  | 'everforest'

export interface AppSettings {
  version: number
  general: GeneralSettings
  appearance: AppearanceSettings
  layout: LayoutSettings
  ai: AISettings
  parser: ParserSettings
  analysis: AnalysisRulesSettings
}

export interface GeneralSettings {
  firstRunCompleted: boolean
}

export type ChatTextSize = 'sm' | 'md' | 'lg'

export interface AppearanceSettings {
  theme: ThemeMode
  density: 'comfortable' | 'compact'
  reduceMotion: boolean
  highContrast: boolean
  chatTextSize: ChatTextSize
}

export interface LayoutSettings {
  sidebarWidth: number
  inspectorWidth: number
  sidebarCollapsed: boolean
  inspectorCollapsed: boolean
  lastActiveInspectorTab: 'details' | 'ai' | 'findings' | 'metrics' | 'sharing'
  lastViewMode: 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin'
}

export interface AIPromptsConfig {
  block: string[]
  flow: string[]
  finding: string[]
  blockWithFindings: string[]
}

export type AIProvidersConfig = Record<KnownProviderID, AIProviderConfig> & {
  [id: ProviderID]: AIProviderConfig | undefined
}

export interface AISettings {
  activeProvider: ProviderID
  embeddingProvider: ProviderID
  embeddingModel?: string
  providers: AIProvidersConfig
  showCostEstimates: boolean
  saveConversationHistory: boolean
  systemPromptSuffix?: string
  dailyBudget: number
  prompts: AIPromptsConfig
}

export type ReasoningEffort = 'low' | 'medium' | 'high'

export interface AIProviderConfig {
  enabled: boolean
  defaultModel: string
  temperature: number
  maxTokens: number
  contextTokenBudget: number
  reasoningEffort?: '' | ReasoningEffort
}

export interface ParserSettings {
  maxFileSizeMB: number
}

export interface AnalysisRulesSettings {
  rules: Record<string, RuleConfig>
  autoAnalyzeOnOpen: boolean
}
