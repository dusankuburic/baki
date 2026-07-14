// App settings tree — what /api/system/settings round-trips between the
// settings store and the backend.

import type {ProviderID} from './providers'
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
  lastUsedVersion: string
  checkForUpdates: 'never' | 'daily' | 'weekly' | 'monthly'
  openInNewWindow: boolean
}

export interface AppearanceSettings {
  theme: ThemeMode
  density: 'comfortable' | 'compact'
  codeFont: string
  uiFont: string
  reduceMotion: boolean
  highContrast: boolean
}

export interface LayoutSettings {
  sidebarWidth: number
  inspectorWidth: number
  sidebarCollapsed: boolean
  inspectorCollapsed: boolean
  lastActiveInspectorTab: 'details' | 'ai' | 'findings' | 'metrics' | 'sharing'
  lastViewMode: 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin'
  chatPanelHeight?: number
}

export interface AIPromptsConfig {
  block: string[]
  flow: string[]
  finding: string[]
  blockWithFindings: string[]
}

export interface AISettings {
  activeProvider: ProviderID
  embeddingProvider: ProviderID
  providers: Record<ProviderID, AIProviderConfig>
  demoMode: DemoModeSettings
  showCostEstimates: boolean
  saveConversationHistory: boolean
  systemPromptSuffix?: string
  dailyBudget: number
  prompts: AIPromptsConfig
}

export interface AIProviderConfig {
  enabled: boolean
  defaultModel: string
  temperature: number
  maxTokens: number
  contextTokenBudget: number
}

export interface DemoModeSettings {
  enabled: boolean
  dailyLimit: number
  dailyUsed: number
  resetDate: string
}

export interface ParserSettings {
  maxFileSizeMB: number
  preserveComments: boolean
  treatTabsAsSpaces: boolean
  spacesPerIndent: number
}

export interface AnalysisRulesSettings {
  rules: Record<string, RuleConfig>
  autoAnalyzeOnOpen: boolean
}
