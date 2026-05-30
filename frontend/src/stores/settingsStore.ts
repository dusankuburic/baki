import {create} from 'zustand'
import type {
  AppSettings, ProviderID, AIProviderConfig,
  AppearanceSettings, LayoutSettings, AISettings,
  GeneralSettings, ParserSettings,
} from '@/types/domain'

import {settingsApi} from '@/api'

type SettingsListener = (settings: AppSettings) => void
const listeners: SettingsListener[] = []

export function onSettingsLoaded(fn: SettingsListener): () => void {
  listeners.push(fn)
  return () => {
    const idx = listeners.indexOf(fn)
    if (idx >= 0) listeners.splice(idx, 1)
  }
}

const defaultSettings: AppSettings = {
  version: 1,
  general: {
    firstRunCompleted: false,
    lastUsedVersion: '',
    checkForUpdates: 'weekly',
    openInNewWindow: false,
  },
  appearance: {
    theme: 'dark',
    density: 'comfortable',
    codeFont: 'JetBrains Mono',
    uiFont: 'Inter',
    reduceMotion: false,
    highContrast: false,
  },
  layout: {
    sidebarWidth: 280,
    inspectorWidth: 320,
    sidebarCollapsed: false,
    inspectorCollapsed: false,
    lastActiveInspectorTab: 'details',
    lastViewMode: 'block',
    // chatPanelHeight: undefined = auto-fill (default)
  },
  ai: {
    activeProvider: 'claude',
    providers: {
      claude: {
        enabled: true,
        defaultModel: 'claude-sonnet-4-5',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      openai: {
        enabled: false,
        defaultModel: 'gpt-5',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      gemini: {
        enabled: false,
        defaultModel: 'gemini-2-5-pro',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      xai: {
        enabled: false,
        defaultModel: 'grok-3-mini',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      glm: {
        enabled: false,
        defaultModel: 'glm-5.1',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      'github-models': {
        enabled: false,
        defaultModel: 'gpt-4o',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      copilot: {
        enabled: false,
        defaultModel: 'gpt-4o',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      demo: {
        enabled: false,
        defaultModel: 'demo',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
    },
    demoMode: {enabled: true, dailyLimit: 5, dailyUsed: 0, resetDate: ''},
    showCostEstimates: true,
    saveConversationHistory: true,
  },
  parser: {
    maxFileSizeMB: 50,
    preserveComments: true,
    treatTabsAsSpaces: true,
    spacesPerIndent: 4,
  },
  analysis: {
    rules: {},
    autoAnalyzeOnOpen: false,
  },
}

interface SettingsState {
  settings: AppSettings
  isLoaded: boolean

  loadFromBackend: () => Promise<void>
  updateSettings: (patch: Partial<AppSettings>) => Promise<void>
  updateGeneral: (general: Partial<GeneralSettings>) => Promise<void>
  updateAppearance: (appearance: Partial<AppearanceSettings>) => Promise<void>
  updateParser: (parser: Partial<ParserSettings>) => Promise<void>
  updateLayout: (layout: Partial<LayoutSettings>) => Promise<void>
  updateAI: (ai: Partial<AISettings>) => Promise<void>
  updateProvider: (id: ProviderID, config: Partial<AIProviderConfig>) => Promise<void>
  resetToDefaults: () => Promise<void>
}

async function persist(settings: AppSettings): Promise<void> {
  await settingsApi.updateSettings(settings as any)
}

export const useSettingsStore = create<SettingsState>((set, get) => ({
  settings: defaultSettings,
  isLoaded: false,

  loadFromBackend: async () => {
    try {
      const loaded = await settingsApi.getSettings() as any as AppSettings
      if (loaded) {
        set({settings: loaded, isLoaded: true})
        listeners.forEach(fn => fn(loaded))
      }
    } catch {
      set({isLoaded: true})
    }
  },

  updateSettings: async (patch) => {
    const prev = get().settings
    const next = {...prev, ...patch}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateGeneral: async (general) => {
    const prev = get().settings
    const next = {...prev, general: {...prev.general, ...general}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateAppearance: async (appearance) => {
    const prev = get().settings
    const next = {...prev, appearance: {...prev.appearance, ...appearance}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateParser: async (parser) => {
    const prev = get().settings
    const next = {...prev, parser: {...prev.parser, ...parser}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateLayout: async (layout) => {
    const prev = get().settings
    const next = {...prev, layout: {...prev.layout, ...layout}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateAI: async (ai) => {
    const prev = get().settings
    const next = {...prev, ai: {...prev.ai, ...ai}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  updateProvider: async (id, config) => {
    const prev = get().settings
    const providers = {...prev.ai.providers, [id]: {...prev.ai.providers[id], ...config}}
    const next = {...prev, ai: {...prev.ai, providers}}
    set({settings: next})
    try { await persist(next) } catch { set({settings: prev}) }
  },

  resetToDefaults: async () => {
    const prev = get().settings
    set({settings: defaultSettings})
    try { await persist(defaultSettings) } catch { set({settings: prev}) }
  },
}))
