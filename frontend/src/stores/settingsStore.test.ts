import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {useSettingsStore, __resetPersistQueueForTest} from './settingsStore'

vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

import {settingsApi} from '@/api'
import {ApiError} from '@/api/client'

const mockGet = settingsApi.getSettings as ReturnType<typeof vi.fn>
const mockUpdate = settingsApi.updateSettings as ReturnType<typeof vi.fn>

const initialState = useSettingsStore.getState()

beforeEach(() => {
  vi.useFakeTimers()
  useSettingsStore.setState(initialState, true)
  __resetPersistQueueForTest()
  vi.resetAllMocks()
  mockGet.mockResolvedValue(null)
  mockUpdate.mockResolvedValue(undefined)
})

afterEach(() => {
  __resetPersistQueueForTest()
  vi.useRealTimers()
})

async function flushPersist() {
  await vi.advanceTimersByTimeAsync(1000)
}

describe('loadFromBackend', () => {
  it('sets isLoaded to true on success', async () => {
    mockGet.mockResolvedValue({...initialState.settings, appearance: {theme: 'light'}})

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().isLoaded).toBe(true)
  })

  it('merges loaded settings into state', async () => {
    mockGet.mockResolvedValue({...initialState.settings, appearance: {theme: 'light'}})

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('light')
  })

  it('sets isLoaded even if the API throws', async () => {
    mockGet.mockRejectedValue(new Error('network'))

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().isLoaded).toBe(true)
  })

  it('preserves defaults when backend returns Go zero-values (0/empty)', async () => {
    mockGet.mockResolvedValue({
      version: 1,
      general: {firstRunCompleted: false},
      appearance: {theme: '', density: '', chatTextSize: ''},
      parser: {maxFileSizeMB: 0},
      layout: {sidebarWidth: 0, inspectorWidth: 0, lastActiveInspectorTab: '', lastViewMode: ''},
      ai: {activeProvider: '', embeddingProvider: ''},
      analysis: {autoAnalyzeOnOpen: false},
    })

    await useSettingsStore.getState().loadFromBackend()
    const s = useSettingsStore.getState().settings
    expect(s.parser.maxFileSizeMB).toBe(50)
    expect(s.appearance.theme).toBe('dark')
    expect(s.appearance.density).toBe('comfortable')
    expect(s.appearance.chatTextSize).toBe('md')
    expect(s.ai.activeProvider).toBeTruthy()
    expect(s.layout.sidebarWidth).toBeGreaterThan(0)
    expect(s.layout.inspectorWidth).toBeGreaterThan(0)
    expect(s.layout.lastActiveInspectorTab).toBeTruthy()
    expect(s.layout.lastViewMode).toBeTruthy()
  })
})

describe('updateAppearance', () => {
  it('updates the appearance theme optimistically', async () => {
    const p = useSettingsStore.getState().updateAppearance({theme: 'midnight'})
    await flushPersist()
    await p
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('midnight')
  })

  it('persists the new settings to the backend', async () => {
    const p = useSettingsStore.getState().updateAppearance({theme: 'nord'})
    await flushPersist()
    await p
    expect(mockUpdate).toHaveBeenCalledOnce()
  })

  it('rolls back to the previous state when persist fails', async () => {
    const originalTheme = useSettingsStore.getState().settings.appearance.theme
    mockUpdate.mockRejectedValue(new Error('disk full'))

    const p = useSettingsStore.getState().updateAppearance({theme: 'dracula'})
    await flushPersist()
    await p.catch(() => {})
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(originalTheme)
  })
})

describe('updateLayout', () => {
  it('updates sidebar width', async () => {
    const p = useSettingsStore.getState().updateLayout({sidebarWidth: 350})
    await flushPersist()
    await p
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(350)
  })

  it('does not touch other layout fields', async () => {
    const orig = useSettingsStore.getState().settings.layout.inspectorWidth
    const p = useSettingsStore.getState().updateLayout({sidebarWidth: 400})
    await flushPersist()
    await p
    expect(useSettingsStore.getState().settings.layout.inspectorWidth).toBe(orig)
  })
})

describe('updateProvider', () => {
  it('updates only the specified provider', async () => {
    const p = useSettingsStore.getState().updateProvider('openai', {enabled: true})
    await flushPersist()
    await p
    const ai = useSettingsStore.getState().settings.ai
    expect(ai.providers.openai.enabled).toBe(true)
    expect(ai.providers.claude.enabled).toBe(true)
  })

  it('merges provider config rather than replacing it', async () => {
    const origTokens = useSettingsStore.getState().settings.ai.providers.claude.maxTokens
    const p = useSettingsStore.getState().updateProvider('claude', {temperature: 0.9})
    await flushPersist()
    await p
    expect(useSettingsStore.getState().settings.ai.providers.claude.temperature).toBe(0.9)
    expect(useSettingsStore.getState().settings.ai.providers.claude.maxTokens).toBe(origTokens)
  })

  it('resolves true once saved', async () => {
    const p = useSettingsStore.getState().updateProvider('azure-foundry', {defaultModel: 'm/a1b2c3d4e5'})
    await flushPersist()
    await expect(p).resolves.toBe(true)
    expect(mockUpdate).toHaveBeenCalledOnce()
  })

  it('reports a failed save and rolls back only the field it wrote', async () => {
    mockUpdate.mockRejectedValue(new Error('disk full'))
    const p = useSettingsStore.getState().updateProvider('azure-foundry', {defaultModel: 'm/a1b2c3d4e5'})
    expect(useSettingsStore.getState().settings.ai.providers['azure-foundry'].defaultModel).toBe('m/a1b2c3d4e5')

    const s = useSettingsStore.getState().settings
    useSettingsStore.setState({
      settings: {
        ...s,
        layout: {...s.layout, sidebarWidth: 333},
        ai: {
          ...s.ai,
          providers: {...s.ai.providers, 'azure-foundry': {...s.ai.providers['azure-foundry'], temperature: 0.7}},
        },
      },
    })

    await flushPersist()
    await expect(p).resolves.toBe(false)
    const after = useSettingsStore.getState().settings
    expect(after.ai.providers['azure-foundry'].defaultModel).toBe('')
    expect(after.ai.providers['azure-foundry'].temperature).toBe(0.7)
    expect(after.layout.sidebarWidth).toBe(333)
  })

  it('leaves a field alone that a later change took over', async () => {
    let failFirst: (err: Error) => void = () => {}
    mockUpdate.mockImplementationOnce(
      () =>
        new Promise<void>((_resolve, reject) => {
          failFirst = reject
        }),
    )
    const store = useSettingsStore.getState()
    const first = store.updateProvider('claude', {defaultModel: 'first'})
    await flushPersist()

    const second = store.updateProvider('claude', {defaultModel: 'second'})
    failFirst(new Error('network'))
    await expect(first).resolves.toBe(false)
    expect(useSettingsStore.getState().settings.ai.providers.claude.defaultModel).toBe('second')

    await flushPersist()
    await expect(second).resolves.toBe(true)
    expect(useSettingsStore.getState().settings.ai.providers.claude.defaultModel).toBe('second')
  })

  it('removes a field on rollback that did not exist before', async () => {
    mockUpdate.mockRejectedValue(new Error('offline'))
    expect(useSettingsStore.getState().settings.ai.providers.claude).not.toHaveProperty('reasoningEffort')
    const p = useSettingsStore.getState().updateProvider('claude', {reasoningEffort: 'high'})
    await flushPersist()
    await expect(p).resolves.toBe(false)
    expect(useSettingsStore.getState().settings.ai.providers.claude).not.toHaveProperty('reasoningEffort')
  })

  it('reports the failure of the write that carried a superseded change', async () => {
    mockUpdate.mockRejectedValue(new Error('disk full'))
    const store = useSettingsStore.getState()
    const origTheme = store.settings.appearance.theme
    const provider = store.updateProvider('openai', {enabled: true})
    const appearance = store.updateAppearance({theme: 'nord'})

    await flushPersist()
    await expect(provider).resolves.toBe(false)
    await appearance
    expect(mockUpdate).toHaveBeenCalledOnce()
    expect(useSettingsStore.getState().settings.ai.providers.openai.enabled).toBe(false)
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(origTheme)
  })
})

describe('resetToDefaults', () => {
  it('restores the default theme', async () => {
    const defaultTheme = initialState.settings.appearance.theme
    const p1 = useSettingsStore.getState().updateAppearance({theme: 'tokyo-night'})
    await flushPersist()
    await p1
    const p2 = useSettingsStore.getState().resetToDefaults()
    await flushPersist()
    await p2
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(defaultTheme)
  })

  it('calls persist with default settings', async () => {
    const p = useSettingsStore.getState().resetToDefaults()
    await flushPersist()
    await p
    expect(mockUpdate).toHaveBeenCalledOnce()
  })
})

describe('persist queue', () => {
  it('coalesces rapid updates into one backend write (debounce)', async () => {
    const store = useSettingsStore.getState()
    const p1 = store.updateAppearance({theme: 'nord'})
    const p2 = store.updateAppearance({theme: 'dracula'})
    const p3 = store.updateAppearance({theme: 'tokyo-night'})

    await flushPersist()
    await Promise.all([p1, p2, p3])

    expect(mockUpdate).toHaveBeenCalledOnce()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('tokyo-night')
  })

  it('resolves a superseded persist immediately rather than hanging', async () => {
    const store = useSettingsStore.getState()
    const superseded = store.updateAppearance({theme: 'nord'})

    const winner = store.updateAppearance({theme: 'dracula'})

    await flushPersist()
    await Promise.all([superseded, winner])

    expect(useSettingsStore.getState().settings.appearance.theme).toBe('dracula')
  })

  it('queues a follow-up write behind an in-flight one', async () => {
    let releaseFirst: () => void = () => {}
    mockUpdate.mockImplementationOnce(
      () =>
        new Promise<void>(resolve => {
          releaseFirst = resolve
        }),
    )

    const store = useSettingsStore.getState()
    const first = store.updateAppearance({theme: 'nord'})
    await flushPersist()

    const second = store.updateAppearance({theme: 'dracula'})
    await flushPersist()

    expect(mockUpdate).toHaveBeenCalledTimes(1)

    releaseFirst()
    await first
    await flushPersist()
    await second

    expect(mockUpdate).toHaveBeenCalledTimes(2)
  })
})

describe('section-only writes (R-frontend-10)', () => {
  const postedKeys = (call = 0) => Object.keys(mockUpdate.mock.calls[call]![0] as object).sort()

  it('posts only the section a write changed, with the document version', async () => {
    const p = useSettingsStore.getState().updateAI({activeProvider: 'gemini'})
    await flushPersist()
    await p
    expect(postedKeys()).toEqual(['ai', 'version'])
  })

  it('posts every section the coalesced writes changed', async () => {
    const store = useSettingsStore.getState()
    const p1 = store.updateAI({activeProvider: 'gemini'})
    const p2 = store.updateLayout({sidebarWidth: 300})
    await flushPersist()
    await Promise.all([p1, p2])
    expect(mockUpdate).toHaveBeenCalledOnce()
    expect(postedKeys()).toEqual(['ai', 'layout', 'version'])
  })

  it('posts the whole document for a reset', async () => {
    const p = useSettingsStore.getState().resetToDefaults()
    await flushPersist()
    await p
    expect(postedKeys()).toEqual(Object.keys(initialState.settings).sort())
  })
})

describe('failed saves (R-frontend-10)', () => {
  it('reports a refused save and re-syncs the document from the server', async () => {
    const server = {...initialState.settings, ai: {...initialState.settings.ai, activeProvider: 'openai'}}
    mockUpdate.mockRejectedValue(
      new ApiError('invalid settings: ai.systemPromptSuffix is 4500 characters, at most 4000', 400, 'BAD_REQUEST'),
    )
    mockGet.mockResolvedValue(server)

    const p = useSettingsStore.getState().updateAI({systemPromptSuffix: 'x'.repeat(4500)})
    await flushPersist()
    await p
    await vi.waitFor(() => expect(mockGet).toHaveBeenCalledOnce())

    const state = useSettingsStore.getState()
    expect(state.saveError).toMatchObject({
      message: 'invalid settings: ai.systemPromptSuffix is 4500 characters, at most 4000',
      sections: ['ai'],
    })
    await vi.waitFor(() => expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('openai'))
    expect(useSettingsStore.getState().settings.ai.systemPromptSuffix ?? '').toBe('')
  })

  it('does not re-sync over a change still waiting to be saved, and keeps it', async () => {
    let rejectFirst: (err: unknown) => void = () => {}
    mockUpdate.mockImplementationOnce(
      () =>
        new Promise<void>((_, reject) => {
          rejectFirst = reject
        }),
    )
    const originalTheme = useSettingsStore.getState().settings.appearance.theme
    const store = useSettingsStore.getState()
    const first = store.updateAppearance({theme: 'nord'})
    await flushPersist()
    const second = store.updateLayout({sidebarWidth: 333})

    rejectFirst(new ApiError('offline', 0, 'NETWORK'))
    await first
    expect(mockGet).not.toHaveBeenCalled()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(originalTheme)
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(333)

    await flushPersist()
    await second
    expect(mockUpdate).toHaveBeenCalledTimes(2)
    expect(Object.keys(mockUpdate.mock.calls[1]![0] as object).sort()).toEqual(['layout', 'version'])
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(333)
  })

  it('clears the error once a later save succeeds', async () => {
    mockUpdate.mockRejectedValueOnce(new ApiError('nope', 400, 'BAD_REQUEST'))
    const p1 = useSettingsStore.getState().updateAI({activeProvider: 'gemini'})
    await flushPersist()
    await p1
    expect(useSettingsStore.getState().saveError).not.toBeNull()

    const p2 = useSettingsStore.getState().updateAI({activeProvider: 'claude'})
    await flushPersist()
    await p2
    expect(useSettingsStore.getState().saveError).toBeNull()
  })
})

describe('refused writes after review (R-frontend-10)', () => {
  it('reports a refused write once, however many changes it carried, and re-reads once', async () => {
    mockUpdate.mockRejectedValue(new ApiError('nope', 400, 'BAD_REQUEST'))
    const errors: unknown[] = []
    const unsub = useSettingsStore.subscribe((s, prev) => {
      if (s.saveError && s.saveError !== prev.saveError) errors.push(s.saveError)
    })
    const store = useSettingsStore.getState()
    const writes = ['a', 'ab', 'abc', 'abcd', 'abcde'].map(v => store.updateAI({systemPromptSuffix: v}))
    await flushPersist()
    await Promise.all(writes)
    await vi.waitFor(() => expect(mockGet).toHaveBeenCalled())
    unsub()
    expect(mockUpdate).toHaveBeenCalledOnce()
    expect(mockGet).toHaveBeenCalledOnce()
    expect(errors).toHaveLength(1)
  })

  it('a re-read from the server keeps a change still waiting to be saved', async () => {
    mockGet.mockResolvedValue({
      ...initialState.settings,
      ai: {...initialState.settings.ai, systemPromptSuffix: 'server'},
    })
    const pending = useSettingsStore.getState().updateAI({systemPromptSuffix: 'mine'})
    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().settings.ai.systemPromptSuffix).toBe('mine')
    await flushPersist()
    await pending
    expect((mockUpdate.mock.calls[0]![0] as {ai: {systemPromptSuffix: string}}).ai.systemPromptSuffix).toBe('mine')
  })

  it('a reset of the queue does not stop later refused writes from re-reading', async () => {
    const dropped = useSettingsStore.getState().updateLayout({sidebarWidth: 301})
    __resetPersistQueueForTest()
    await dropped
    mockUpdate.mockRejectedValue(new ApiError('nope', 400, 'BAD_REQUEST'))
    const p = useSettingsStore.getState().updateAI({activeProvider: 'gemini'})
    await flushPersist()
    await p
    await vi.waitFor(() => expect(mockGet).toHaveBeenCalledOnce())
  })

  it('puts back only the fields the refused write changed, and never posts them again', async () => {
    let rejectFirst: (err: unknown) => void = () => {}
    mockUpdate.mockImplementationOnce(
      () =>
        new Promise<void>((_, reject) => {
          rejectFirst = reject
        }),
    )
    const store = useSettingsStore.getState()
    const first = store.updateAI({systemPromptSuffix: 'refused'})
    await flushPersist()
    const second = store.updateAI({activeProvider: 'gemini'})
    rejectFirst(new ApiError('invalid settings: ai.systemPromptSuffix', 400, 'BAD_REQUEST'))
    await first
    expect(useSettingsStore.getState().settings.ai.systemPromptSuffix ?? '').toBe('')
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('gemini')
    await flushPersist()
    await second
    const body = mockUpdate.mock.calls[1]![0] as {ai: {systemPromptSuffix?: string; activeProvider: string}}
    expect(body.ai.activeProvider).toBe('gemini')
    expect(body.ai.systemPromptSuffix ?? '').toBe('')
  })

  it('keeps an ai refusal until an ai save succeeds', async () => {
    mockUpdate.mockRejectedValueOnce(new ApiError('nope', 400, 'BAD_REQUEST'))
    const p1 = useSettingsStore.getState().updateAI({activeProvider: 'gemini'})
    await flushPersist()
    await p1
    const p2 = useSettingsStore.getState().updateLayout({sidebarWidth: 290})
    await flushPersist()
    await p2
    expect(useSettingsStore.getState().saveError?.sections).toEqual(['ai'])
    const p3 = useSettingsStore.getState().updateAI({activeProvider: 'claude'})
    await flushPersist()
    await p3
    expect(useSettingsStore.getState().saveError).toBeNull()
  })

  it('puts a refused layout write back without a report, and ignores an aborted write', async () => {
    mockUpdate.mockRejectedValueOnce(new ApiError('offline', 0, 'NETWORK'))
    const p1 = useSettingsStore.getState().updateLayout({sidebarWidth: 290})
    await flushPersist()
    await p1
    expect(useSettingsStore.getState().saveError).toBeNull()
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(initialState.settings.layout.sidebarWidth)

    mockUpdate.mockRejectedValueOnce(new DOMException('Aborted', 'AbortError'))
    const p2 = useSettingsStore.getState().updateAI({activeProvider: 'gemini'})
    await flushPersist()
    await p2
    expect(useSettingsStore.getState().saveError).toBeNull()
  })
})
