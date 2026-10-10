import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {
  AppSettings,
  ProviderID,
  AIProviderConfig,
  AppearanceSettings,
  LayoutSettings,
  AISettings,
  GeneralSettings,
  ParserSettings,
} from '@/types'

import {settingsApi} from '@/api'
import {logger} from '@/lib/logger'

type SettingsListener = (settings: AppSettings) => void
const listeners: SettingsListener[] = []

export function onSettingsLoaded(fn: SettingsListener): () => void {
  listeners.push(fn)
  return () => {
    const idx = listeners.indexOf(fn)
    if (idx >= 0) listeners.splice(idx, 1)
  }
}

// What a provider the catalogue does not know is tuned from until the user
// changes something: the same numbers as every catalogue entry, no default
// model (the server's wins).
export const DEFAULT_PROVIDER_CONFIG: AIProviderConfig = {
  enabled: false,
  defaultModel: '',
  temperature: 0.3,
  maxTokens: 4096,
  contextTokenBudget: 4000,
}

/** A provider's tuning, the defaults for one without an entry (a provider a newer server added). */
export function providerConfig(ai: Pick<AISettings, 'providers'>, id: ProviderID): AIProviderConfig {
  return ai.providers[id] ?? DEFAULT_PROVIDER_CONFIG
}

export const defaultSettings: AppSettings = {
  version: 1,
  general: {
    firstRunCompleted: false,
  },
  appearance: {
    theme: 'dark',
    density: 'comfortable',
    reduceMotion: false,
    highContrast: false,
    chatTextSize: 'md',
  },
  layout: {
    sidebarWidth: 280,
    inspectorWidth: 320,
    sidebarCollapsed: false,
    inspectorCollapsed: false,
    lastActiveInspectorTab: 'details',
    lastViewMode: 'block',
  },
  ai: {
    activeProvider: 'claude',
    embeddingProvider: 'openai',
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
        defaultModel: 'glm-5.2',
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
      // Empty until the user picks a model in the chat header; the
      // server-reported default (the saved chat deployment) wins until then.
      'azure-foundry': {
        enabled: false,
        defaultModel: '',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      // Empty until the user picks one of the local server's models in the
      // chat header; the server-reported default wins until then.
      local: {
        enabled: false,
        defaultModel: '',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
    },
    showCostEstimates: true,
    saveConversationHistory: true,
    dailyBudget: 5.0,
    prompts: {
      block: [
        'Explain this block',
        'Find issues here',
        'Suggest improvements',
        'What does this block do?',
        'Could this block cause errors?',
      ],
      flow: [
        'Analyze the whole flow',
        'Find performance issues',
        'Security audit',
        'Find potential bugs',
        'Summarize what this flow does',
      ],
      finding: ['How do I fix this issue?', 'Is this a false positive?', 'Show me similar patterns in the flow'],
      blockWithFindings: [
        'How do I fix the issues on this block?',
        'Are these findings related?',
        'Is this a false positive?',
        'Show me similar patterns in the flow',
        'Explain what this block does',
      ],
    },
  },
  parser: {
    maxFileSizeMB: 50,
  },
  analysis: {
    rules: {},
    autoAnalyzeOnOpen: true,
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
  /**
   * Optimistically merges `config` into the provider's settings. Resolves
   * true once saved, false when the save failed: the fields this call wrote
   * are then rolled back — only those, and only where they still hold this
   * call's value, so a change made meanwhile (to them or anything else) stays.
   * Never rejects.
   */
  updateProvider: (id: ProviderID, config: Partial<AIProviderConfig>) => Promise<boolean>
  resetToDefaults: () => Promise<void>
}

// Persist with a debounce to prevent flooding the backend during rapid UI
// updates (like window resizing or slider dragging). Rejects on failure so
// callers can roll back their optimistic update. A superseded persist settles
// with the write that supersedes it: the newer call reads the latest state,
// so it carries this call's changes too — and if that write fails, this
// call's change was not saved either. The callers of one write settle newest
// first, so their rollbacks unwind like a stack (the oldest caller's
// snapshot restore runs last).
//
// An in-flight guard prevents two concurrent API calls: if a new persist
// arrives while the previous one is still hitting the network, the new call
// waits for the previous to settle, then retries with the latest settings.
//
// The three pieces of queue state live on one object (persistQueue) rather than
// three loose module-level vars so a test-only reset can clear them atomically
// (otherwise the debounce timer / in-flight promise leak across test cases).
interface PersistWaiter {
  resolve: () => void
  reject: (err: unknown) => void
}
interface PersistQueueState {
  timer: ReturnType<typeof setTimeout> | null
  // The callers whose changes the pending (debounced) write carries.
  waiters: PersistWaiter[]
  inflight: Promise<void> | null
}
const persistQueue: PersistQueueState = {
  timer: null,
  waiters: [],
  inflight: null,
}

// __resetPersistQueueForTest clears the debounce timer + in-flight/waiting
// state so unit tests don't leak them across cases. Production code must not
// call this: it would drop a pending write.
export function __resetPersistQueueForTest(): void {
  if (persistQueue.timer) {
    clearTimeout(persistQueue.timer)
  }
  persistQueue.timer = null
  // Resolve every waiting caller so none hangs the test.
  const waiters = persistQueue.waiters
  persistQueue.waiters = []
  for (const w of waiters) w.resolve()
  persistQueue.inflight = null
}

async function persist(settings: AppSettings): Promise<void> {
  if (persistQueue.inflight) {
    await persistQueue.inflight.catch(() => {})
    return persist(settings)
  }

  if (persistQueue.timer) {
    clearTimeout(persistQueue.timer)
  }

  return new Promise((resolve, reject) => {
    persistQueue.waiters.push({resolve, reject})
    persistQueue.timer = setTimeout(() => {
      persistQueue.timer = null
      // Newest first (see above).
      const waiters = [...persistQueue.waiters].reverse()
      persistQueue.waiters = []
      persistQueue.inflight = settingsApi
        .updateSettings(settings)
        .then(() => {
          for (const w of waiters) w.resolve()
        })
        .catch(err => {
          logger.warn('Failed to persist settings', err)
          for (const w of waiters) w.reject(err)
        })
        .finally(() => {
          persistQueue.inflight = null
        })
    }, 1000)
  })
}

export const useSettingsStore = create<SettingsState>((set, get) => {
  // Optimistically apply `next`, persist, and restore `prev` if the backend
  // write fails (persist already logs the error).
  const applyAndPersist = async (next: AppSettings, prev: AppSettings) => {
    set({settings: next})
    try {
      await persist(next)
    } catch {
      set({settings: prev})
    }
  }

  return {
    settings: defaultSettings,
    isLoaded: false,

    loadFromBackend: async () => {
      try {
        const loaded = await settingsApi.getSettings()
        if (loaded) {
          // Deep-merge server response with defaults so Go zero-values (0 for
          // int fields, "" for strings) don't clobber the frontend defaults that
          // guarantee a usable experience on first run. Each scalar with a
          // non-zero default gets an explicit || fallback.
          const merged: AppSettings = {
            ...defaultSettings,
            ...loaded,
            general: {
              ...defaultSettings.general,
              ...loaded.general,
            },
            appearance: {
              ...defaultSettings.appearance,
              ...loaded.appearance,
              theme: loaded.appearance?.theme || defaultSettings.appearance.theme,
              density: loaded.appearance?.density || defaultSettings.appearance.density,
              chatTextSize: loaded.appearance?.chatTextSize || defaultSettings.appearance.chatTextSize,
            },
            parser: {
              ...defaultSettings.parser,
              ...loaded.parser,
              maxFileSizeMB: loaded.parser?.maxFileSizeMB || defaultSettings.parser.maxFileSizeMB,
            },
            layout: {
              ...defaultSettings.layout,
              ...loaded.layout,
              sidebarWidth: loaded.layout?.sidebarWidth || defaultSettings.layout.sidebarWidth,
              inspectorWidth: loaded.layout?.inspectorWidth || defaultSettings.layout.inspectorWidth,
              lastActiveInspectorTab:
                loaded.layout?.lastActiveInspectorTab || defaultSettings.layout.lastActiveInspectorTab,
              lastViewMode: loaded.layout?.lastViewMode || defaultSettings.layout.lastViewMode,
            },
            ai: {
              ...defaultSettings.ai,
              ...loaded.ai,
              activeProvider: loaded.ai?.activeProvider || defaultSettings.ai.activeProvider,
              embeddingProvider: loaded.ai?.embeddingProvider || defaultSettings.ai.embeddingProvider,
              dailyBudget: loaded.ai?.dailyBudget || defaultSettings.ai.dailyBudget,
              providers: {
                ...defaultSettings.ai.providers,
                ...(loaded.ai?.providers ?? {}),
              },
              prompts: {
                block: loaded.ai?.prompts?.block ?? defaultSettings.ai.prompts.block,
                flow: loaded.ai?.prompts?.flow ?? defaultSettings.ai.prompts.flow,
                finding: loaded.ai?.prompts?.finding ?? defaultSettings.ai.prompts.finding,
                blockWithFindings:
                  loaded.ai?.prompts?.blockWithFindings ?? defaultSettings.ai.prompts.blockWithFindings,
              },
            },
            analysis: {
              ...defaultSettings.analysis,
              ...loaded.analysis,
              rules: {
                ...defaultSettings.analysis.rules,
                ...(loaded.analysis?.rules ?? {}),
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

    updateSettings: async patch => {
      const prev = get().settings
      await applyAndPersist({...prev, ...patch}, prev)
    },

    updateGeneral: async general => {
      const prev = get().settings
      await applyAndPersist({...prev, general: {...prev.general, ...general}}, prev)
    },

    updateAppearance: async appearance => {
      const prev = get().settings
      await applyAndPersist({...prev, appearance: {...prev.appearance, ...appearance}}, prev)
    },

    updateParser: async parser => {
      const prev = get().settings
      await applyAndPersist({...prev, parser: {...prev.parser, ...parser}}, prev)
    },

    updateLayout: async layout => {
      const prev = get().settings
      await applyAndPersist({...prev, layout: {...prev.layout, ...layout}}, prev)
    },

    updateAI: async ai => {
      const prev = get().settings
      await applyAndPersist({...prev, ai: {...prev.ai, ...ai}}, prev)
    },

    updateProvider: async (id, config) => {
      const prev = get().settings
      const before: AIProviderConfig | undefined = prev.ai.providers[id]
      // A provider without an entry (one a newer server added) gets a whole
      // one: the defaults with this change on top.
      const entry = {...(before ?? DEFAULT_PROVIDER_CONFIG), ...config}
      const next = {...prev, ai: {...prev.ai, providers: {...prev.ai.providers, [id]: entry}}}
      set({settings: next})
      try {
        await persist(next)
        return true
      } catch {
        // Unlike applyAndPersist's whole-settings snapshot, which would also
        // undo every change made while this write was pending, restore only
        // the fields this call wrote that still hold its value.
        const cur = get().settings
        const now: AIProviderConfig | undefined = cur.ai.providers[id]
        const restored: Record<string, unknown> = {...now}
        let rolledBack = false
        for (const field of Object.keys(config) as (keyof AIProviderConfig)[]) {
          if (!Object.is(now?.[field], config[field])) continue
          rolledBack = true
          if (before && field in before) restored[field] = before[field]
          else delete restored[field]
        }
        if (rolledBack) {
          const providers = {...cur.ai.providers, [id]: restored as unknown as AIProviderConfig}
          set({settings: {...cur, ai: {...cur.ai, providers}}})
        }
        return false
      }
    },

    resetToDefaults: async () => {
      const prev = get().settings
      await applyAndPersist(defaultSettings, prev)
    },
  }
})

// Reset on logout (see storeRegistry). Settings repopulate from the backend on
// the next login (loadFromBackend).
registerStoreReset(() => useSettingsStore.setState({settings: defaultSettings, isLoaded: false}))
