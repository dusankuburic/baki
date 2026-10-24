
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
  profile: ProfileSettings
  general: GeneralSettings
  appearance: AppearanceSettings
  layout: LayoutSettings
  ai: AISettings
  parser: ParserSettings
  analysis: AnalysisRulesSettings
}

/** The user's identity card beyond the account row; all fields optional (core/models/settings.go ProfileSettings). */
export interface ProfileSettings {
  bio?: string
  jobTitle?: string
  /** IANA zone name; empty keeps the device's. */
  timezone?: string
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
  saveConversationHistory: boolean
  systemPromptSuffix?: string
  /** An organisation's allowed providers (ADR 0028): absent or empty allows every provider. The org settings
   * route keeps the stored list when a write leaves this out, and clears it on an explicit []. */
  allowedProviders?: ProviderID[]
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
