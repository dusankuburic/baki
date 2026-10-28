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
  ProfileSettings,
} from '@/types'

import {settingsApi} from '@/api'
import {logger} from '@/lib/logger'
import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {setDisplayTimeZone} from '@/lib/time'

type SettingsListener = (settings: AppSettings) => void
const listeners: SettingsListener[] = []

export function onSettingsLoaded(fn: SettingsListener): () => void {
  listeners.push(fn)
  return () => {
    const idx = listeners.indexOf(fn)
    if (idx >= 0) listeners.splice(idx, 1)
  }
}

export const DEFAULT_PROVIDER_CONFIG: AIProviderConfig = {
  enabled: false,
  defaultModel: '',
  temperature: 0.3,
  maxTokens: 4096,
  contextTokenBudget: 4000,
}

export function providerConfig(ai: Pick<AISettings, 'providers'>, id: ProviderID): AIProviderConfig {
  return ai.providers[id] ?? DEFAULT_PROVIDER_CONFIG
}

export const defaultSettings: AppSettings = {
  version: 1,
  profile: {
    bio: '',
    jobTitle: '',
    timezone: '',
  },
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
      'azure-foundry': {
        enabled: false,
        defaultModel: '',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
      local: {
        enabled: false,
        defaultModel: '',
        temperature: 0.3,
        maxTokens: 4096,
        contextTokenBudget: 4000,
      },
    },
    saveConversationHistory: true,
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

/** The last refused save: the server's (or the network's) message and the sections the write carried. The
 * document has been put back and re-read from the server; a later save that succeeds clears it. */
export interface SettingsSaveError {
  message: string
  code: string | null
  sections: SettingsSection[]
  at: number
}

/** A top-level section of the settings document; a write posts only the sections it changed. */
export type SettingsSection = Exclude<keyof AppSettings, 'version'>

interface SettingsState {
  settings: AppSettings
  isLoaded: boolean
  saveError: SettingsSaveError | null

  loadFromBackend: () => Promise<void>
  updateSettings: (patch: Partial<AppSettings>) => Promise<void>
  updateProfile: (profile: Partial<ProfileSettings>) => Promise<void>
  updateGeneral: (general: Partial<GeneralSettings>) => Promise<void>
  updateAppearance: (appearance: Partial<AppearanceSettings>) => Promise<void>
  updateParser: (parser: Partial<ParserSettings>) => Promise<void>
  updateLayout: (layout: Partial<LayoutSettings>) => Promise<void>
  updateAI: (ai: Partial<AISettings>) => Promise<void>
  updateProvider: (id: ProviderID, config: Partial<AIProviderConfig>) => Promise<boolean>
  resetToDefaults: () => Promise<void>
}

interface PersistWaiter {
  resolve: () => void
  reject: (err: unknown) => void
}
interface PersistQueueState {
  timer: ReturnType<typeof setTimeout> | null
  waiters: PersistWaiter[]
  inflight: Promise<void> | null
  /** The sections the debounced write will post; 'all' for a whole-document write (a reset). */
  sections: Set<SettingsSection> | 'all'
  /** The sections of the writes not yet settled (debounced, in flight, or waiting behind the one in flight), each
   * with its count: a re-read from the server keeps the store's own value for these. */
  pending: Map<SettingsSection, number>
  /** Bumped by a reset: a write from before it neither counts nor reports. */
  generation: number
  /** A refused write asked for a re-read, made once no write is pending. */
  resyncWanted: boolean
}
const persistQueue: PersistQueueState = {
  timer: null,
  waiters: [],
  inflight: null,
  sections: new Set(),
  pending: new Map(),
  generation: 0,
  resyncWanted: false,
}

/** What the queue asks of the store (set once the store exists): its current document, and what a flush's outcome
 * means for it. */
const storeHooks: {
  current: () => AppSettings
  refused: (err: unknown, sections: SettingsSection[]) => void
  saved: (sections: SettingsSection[]) => void
  resync: () => void
} = {current: () => defaultSettings, refused: () => {}, saved: () => {}, resync: () => {}}

const ALL_SECTIONS = Object.keys(defaultSettings).filter(k => k !== 'version') as SettingsSection[]

function dropPendingPersist(): void {
  if (persistQueue.timer) {
    clearTimeout(persistQueue.timer)
  }
  persistQueue.timer = null
  const waiters = persistQueue.waiters
  persistQueue.waiters = []
  for (const w of waiters) w.resolve()
  persistQueue.inflight = null
  persistQueue.sections = new Set()
  persistQueue.pending = new Map()
  persistQueue.generation++
  persistQueue.resyncWanted = false
}

function pendingSections(): Set<SettingsSection> {
  return new Set(persistQueue.pending.keys())
}

/** The body of a write: the document version and the named sections (the server keeps the others as stored, so
 * two tabs editing different sections do not overwrite each other), or the whole document. */
function writeBody(settings: AppSettings, sections: Set<SettingsSection> | 'all'): AppSettings {
  if (sections === 'all') return settings
  const body: Record<string, unknown> = {version: settings.version}
  for (const key of sections) body[key] = settings[key]
  return body as unknown as AppSettings
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError'
}

export function __resetPersistQueueForTest(): void {
  dropPendingPersist()
}

async function persist(sections: SettingsSection[] | 'all'): Promise<void> {
  const generation = persistQueue.generation
  const names = sections === 'all' ? ALL_SECTIONS : sections
  for (const key of names) persistQueue.pending.set(key, (persistQueue.pending.get(key) ?? 0) + 1)
  try {
    await persistQueued(sections)
  } finally {
    if (generation === persistQueue.generation) {
      for (const key of names) {
        const n = (persistQueue.pending.get(key) ?? 1) - 1
        if (n > 0) persistQueue.pending.set(key, n)
        else persistQueue.pending.delete(key)
      }
      if (persistQueue.pending.size === 0 && persistQueue.resyncWanted) {
        persistQueue.resyncWanted = false
        storeHooks.resync()
      }
    }
  }
}

async function persistQueued(sections: SettingsSection[] | 'all'): Promise<void> {
  if (persistQueue.inflight) {
    await persistQueue.inflight.catch(() => {})
    return persistQueued(sections)
  }

  if (persistQueue.timer) {
    clearTimeout(persistQueue.timer)
  }
  if (sections === 'all') persistQueue.sections = 'all'
  else if (persistQueue.sections !== 'all') for (const key of sections) persistQueue.sections.add(key)

  return new Promise((resolve, reject) => {
    persistQueue.waiters.push({resolve, reject})
    persistQueue.timer = setTimeout(() => {
      persistQueue.timer = null
      const waiters = [...persistQueue.waiters].reverse()
      persistQueue.waiters = []
      const named = persistQueue.sections
      persistQueue.sections = new Set()
      const flushed = named === 'all' ? ALL_SECTIONS : [...named]
      const generation = persistQueue.generation
      // The store as it is now, not as the first coalesced write saw it: a refused earlier write has been put back
      // since, and posting its value again would save what the user was told was not saved.
      persistQueue.inflight = settingsApi
        .updateSettings(writeBody(storeHooks.current(), named))
        .then(() => {
          if (generation === persistQueue.generation) storeHooks.saved(flushed)
          for (const w of waiters) w.resolve()
        })
        .catch(err => {
          logger.warn('Failed to persist settings', err)
          // One report per refused write, however many changes it carried.
          if (generation === persistQueue.generation && !isAbort(err)) storeHooks.refused(err, flushed)
          for (const w of waiters) w.reject(err)
        })
        .finally(() => {
          persistQueue.inflight = null
        })
    }, 1000)
  })
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

/** cur with each field a refused write changed (from prev to next) put back to prev, unless a later change has
 * taken that field over since; a section the write replaced whole goes back whole while it is still the write's. */
function rollBack(cur: AppSettings, next: AppSettings, prev: AppSettings, sections: SettingsSection[]): AppSettings {
  const out: Record<string, unknown> = {...cur}
  for (const key of sections) {
    const c: unknown = cur[key]
    const n: unknown = next[key]
    const p: unknown = prev[key]
    if (Object.is(c, n)) {
      out[key] = p
      continue
    }
    if (!isPlainObject(c) || !isPlainObject(n) || !isPlainObject(p)) continue
    const section = {...c}
    let changed = false
    for (const field of new Set([...Object.keys(n), ...Object.keys(p)])) {
      if (Object.is(n[field], p[field]) || !Object.is(c[field], n[field])) continue
      changed = true
      if (field in p) section[field] = p[field]
      else delete section[field]
    }
    if (changed) out[key] = section
  }
  return out as unknown as AppSettings
}

export const useSettingsStore = create<SettingsState>((set, get) => {
  // A refused write is reported once, by the flush (storeHooks.refused): the toast and the panels' alert show it,
  // and the document is re-read once no write is pending. A layout write (pane sizes, the last view) is put back
  // without a report. A save that covers the reported sections clears it.
  storeHooks.current = () => get().settings
  storeHooks.refused = (err, sections) => {
    persistQueue.resyncWanted = true
    if (sections.every(key => key === 'layout')) return
    set({saveError: {message: getErrorMessage(err, ''), code: getErrorCode(err), sections, at: Date.now()}})
  }
  storeHooks.saved = sections => {
    const error = get().saveError
    if (error && error.sections.some(key => sections.includes(key))) set({saveError: null})
  }
  storeHooks.resync = () => void get().loadFromBackend()

  const applyAndPersist = async (next: AppSettings, prev: AppSettings, sections?: SettingsSection[]) => {
    set({settings: next})
    try {
      await persist(sections ?? 'all')
    } catch {
      set({settings: rollBack(get().settings, next, prev, sections ?? ALL_SECTIONS)})
    }
  }

  return {
    settings: defaultSettings,
    isLoaded: false,
    saveError: null,

    loadFromBackend: async () => {
      try {
        const loaded = await settingsApi.getSettings()
        if (loaded) {
          const merged: AppSettings = {
            ...defaultSettings,
            ...loaded,
            profile: {
              ...defaultSettings.profile,
              ...loaded.profile,
            },
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
          // A section with a write still pending keeps the store's value: the write is about to save it, and the
          // server's (read before it) would show the user's change undone.
          const current = get().settings as unknown as Record<string, unknown>
          const kept = merged as unknown as Record<string, unknown>
          for (const key of pendingSections()) kept[key] = current[key]
          set({settings: merged, isLoaded: true})
          listeners.forEach(fn => fn(merged))
        }
      } catch {
        set({isLoaded: true})
      }
    },

    updateSettings: async patch => {
      const prev = get().settings
      const sections = Object.keys(patch).filter(k => k !== 'version') as SettingsSection[]
      await applyAndPersist({...prev, ...patch}, prev, sections)
    },

    updateProfile: async profile => {
      const prev = get().settings
      await applyAndPersist({...prev, profile: {...prev.profile, ...profile}}, prev, ['profile'])
    },

    updateGeneral: async general => {
      const prev = get().settings
      await applyAndPersist({...prev, general: {...prev.general, ...general}}, prev, ['general'])
    },

    updateAppearance: async appearance => {
      const prev = get().settings
      await applyAndPersist({...prev, appearance: {...prev.appearance, ...appearance}}, prev, ['appearance'])
    },

    updateParser: async parser => {
      const prev = get().settings
      await applyAndPersist({...prev, parser: {...prev.parser, ...parser}}, prev, ['parser'])
    },

    updateLayout: async layout => {
      const prev = get().settings
      await applyAndPersist({...prev, layout: {...prev.layout, ...layout}}, prev, ['layout'])
    },

    updateAI: async ai => {
      const prev = get().settings
      await applyAndPersist({...prev, ai: {...prev.ai, ...ai}}, prev, ['ai'])
    },

    updateProvider: async (id, config) => {
      const prev = get().settings
      const before: AIProviderConfig | undefined = prev.ai.providers[id]
      const entry = {...(before ?? DEFAULT_PROVIDER_CONFIG), ...config}
      const next = {...prev, ai: {...prev.ai, providers: {...prev.ai.providers, [id]: entry}}}
      set({settings: next})
      try {
        await persist(['ai'])
        return true
      } catch {
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

// One display zone for every timestamp: the profile's choice, or the device's while it is empty.
useSettingsStore.subscribe(s => setDisplayTimeZone(s.settings.profile?.timezone))

registerStoreReset(() => {
  dropPendingPersist()
  useSettingsStore.setState({settings: defaultSettings, isLoaded: false, saveError: null})
})
