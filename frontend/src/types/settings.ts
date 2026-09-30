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

export interface AISettings {
  activeProvider: ProviderID
  embeddingProvider: ProviderID
  /** Override the embedding model name; empty keeps the provider default. */
  embeddingModel?: string
  providers: Record<ProviderID, AIProviderConfig>
  showCostEstimates: boolean
  saveConversationHistory: boolean
  systemPromptSuffix?: string
  dailyBudget: number
  prompts: AIPromptsConfig
}

// Reasoning depth for reasoning deployments (F-19).
export type ReasoningEffort = 'low' | 'medium' | 'high'

export interface AIProviderConfig {
  enabled: boolean
  defaultModel: string
  temperature: number
  maxTokens: number
  contextTokenBudget: number
  /**
   * Reasoning depth for reasoning deployments: '' (or omitted) keeps the
   * provider default; otherwise 'low' | 'medium' | 'high'. Sent with each
   * chat turn next to temperature/maxTokens (F-19); models without
   * reasoning ignore it (one repair round trip at worst).
   */
  reasoningEffort?: '' | ReasoningEffort
}

export interface ParserSettings {
  maxFileSizeMB: number
}

export interface AnalysisRulesSettings {
  rules: Record<string, RuleConfig>
  autoAnalyzeOnOpen: boolean
}
