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
        defaultModel: 'claude-sonnet-4-6',
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
    dailyBudget: 5.0,
    prompts: {
      block: [
        "Explain this block",
        "Find issues here",
        "Suggest improvements",
        "What does this block do?",
        "Could this block cause errors?",
      ],
      flow: [
        "Analyze the whole flow",
        "Find performance issues",
        "Security audit",
        "Find potential bugs",
        "Summarize what this flow does",
      ],
      finding: [
        "How do I fix this issue?",
        "Is this a false positive?",
        "Show me similar patterns in the flow",
      ],
      blockWithFindings: [
        "How do I fix the issues on this block?",
        "Are these findings related?",
        "Is this a false positive?",
        "Show me similar patterns in the flow",
        "Explain what this block does",
      ],
    },
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

// Persist with a debounce to prevent flooding the backend during rapid UI
// updates (like window resizing or slider dragging).
let persistTimer: any = null
async function persist(settings: AppSettings): Promise<void> {
  if (persistTimer) {
    clearTimeout(persistTimer)
  }

  return new Promise((resolve) => {
    persistTimer = setTimeout(async () => {
      try {
        await settingsApi.updateSettings(settings as any)
      } catch (err) {
        console.error('Failed to persist settings', err)
      } finally {
        persistTimer = null
        resolve()
      }
    }, 1000) // 1 second debounce
  })
}

export const useSettingsStore = create<SettingsState>((set, get) => ({
  settings: defaultSettings,
  isLoaded: false,

  loadFromBackend: async () => {
    try {
      const loaded = await settingsApi.getSettings() as any as AppSettings
      if (loaded) {
        // Deep-merge server response with defaults so Go zero-values (0 for
        // int fields like sidebarWidth/inspectorWidth) don't clobber the
        // frontend defaults that guarantee a usable layout on first run.
        const merged: AppSettings = {
          ...defaultSettings,
          ...loaded,
          layout: {
            ...defaultSettings.layout,
            ...loaded.layout,
            sidebarWidth:   loaded.layout?.sidebarWidth   || defaultSettings.layout.sidebarWidth,
            inspectorWidth: loaded.layout?.inspectorWidth || defaultSettings.layout.inspectorWidth,
          },
          ai: {
            ...defaultSettings.ai,
            ...loaded.ai,
            providers: {
              ...defaultSettings.ai.providers,
              ...(loaded.ai?.providers ?? {}),
            },
            // Each prompt list falls back to defaults when the server sends
            // null/undefined — settings persisted before the prompts field
            // existed come back as Go nil slices (JSON null), and a shallow
            // spread would let those nulls clobber the defaults, making the
            // AI Prompts panel map over a null array and crash.
            prompts: {
              block:             loaded.ai?.prompts?.block             ?? defaultSettings.ai.prompts.block,
              flow:              loaded.ai?.prompts?.flow              ?? defaultSettings.ai.prompts.flow,
              finding:           loaded.ai?.prompts?.finding           ?? defaultSettings.ai.prompts.finding,
              blockWithFindings: loaded.ai?.prompts?.blockWithFindings ?? defaultSettings.ai.prompts.blockWithFindings,
            },
          },
        }
        set({settings: merged, isLoaded: true})
        listeners.forEach(fn => fn(merged))
      }
    } catch {
      set({isLoaded: true})
    }
  },

  updateSettings: async (patch) => {
    const prev = get().settings
    const next = {...prev, ...patch}
    set({settings: next})
    await persist(next)
  },

  updateGeneral: async (general) => {
    const prev = get().settings
    const next = {...prev, general: {...prev.general, ...general}}
    set({settings: next})
    await persist(next)
  },

  updateAppearance: async (appearance) => {
    const prev = get().settings
    const next = {...prev, appearance: {...prev.appearance, ...appearance}}
    set({settings: next})
    await persist(next)
  },

  updateParser: async (parser) => {
    const prev = get().settings
    const next = {...prev, parser: {...prev.parser, ...parser}}
    set({settings: next})
    await persist(next)
  },

  updateLayout: async (layout) => {
    const prev = get().settings
    const next = {...prev, layout: {...prev.layout, ...layout}}
    set({settings: next})
    await persist(next)
  },

  updateAI: async (ai) => {
    const prev = get().settings
    const next = {...prev, ai: {...prev.ai, ...ai}}
    set({settings: next})
    await persist(next)
  },

  updateProvider: async (id, config) => {
    const prev = get().settings
    const providers = {...prev.ai.providers, [id]: {...prev.ai.providers[id], ...config}}
    const next = {...prev, ai: {...prev.ai, providers}}
    set({settings: next})
    await persist(next)
  },

  resetToDefaults: async () => {
    set({settings: defaultSettings})
    await persist(defaultSettings)
  },
}))
