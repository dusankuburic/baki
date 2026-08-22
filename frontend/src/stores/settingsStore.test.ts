import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {useSettingsStore, __resetPersistQueueForTest} from './settingsStore'

// Stub out the API layer so tests don't need a real backend
vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

import {settingsApi} from '@/api'

const mockGet = settingsApi.getSettings as ReturnType<typeof vi.fn>
const mockUpdate = settingsApi.updateSettings as ReturnType<typeof vi.fn>

// Snapshot of the store in its initial state — captured before any test mutates it.
// Zustand keeps plain object state; using `setState(snapshot, true)` fully restores it.
const initialState = useSettingsStore.getState()

beforeEach(() => {
  // Use fake timers so the 1-second persist debounce doesn't slow down the suite.
  vi.useFakeTimers()
  // Full replacement so no settings bleed from previous test
  useSettingsStore.setState(initialState, true)
  // Clear any debounce timer / in-flight promise a previous case left behind —
  // without this the module-level persist queue leaks across cases.
  __resetPersistQueueForTest()
  // Reset mock call history AND implementation back to defaults
  vi.resetAllMocks()
  mockGet.mockResolvedValue(null)
  mockUpdate.mockResolvedValue(undefined)
})

afterEach(() => {
  // Belt-and-braces: ensure no queue state escapes this case even if an
  // assertion threw before the timer fired.
  __resetPersistQueueForTest()
  vi.useRealTimers()
})

// Advance fake clock past the 1-second debounce and flush the resulting
// microtasks so the persist promise settles before assertions run.
async function flushPersist() {
  await vi.advanceTimersByTimeAsync(1000)
}

// ---- loadFromBackend ----

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
    // Simulate a first-time cloud user where Postgres returns zero-values
    mockGet.mockResolvedValue({
      version: 1,
      general: {checkForUpdates: ''},
      appearance: {theme: '', density: '', codeFont: '', uiFont: ''},
      parser: {maxFileSizeMB: 0, spacesPerIndent: 0},
      layout: {sidebarWidth: 0, inspectorWidth: 0, lastActiveInspectorTab: '', lastViewMode: ''},
      ai: {activeProvider: '', embeddingProvider: '', dailyBudget: 0, demoMode: {dailyLimit: 0}},
      analysis: {autoAnalyzeOnOpen: false},
    })

    await useSettingsStore.getState().loadFromBackend()
    const s = useSettingsStore.getState().settings
    // Every field must fall back to its non-zero default
    expect(s.parser.maxFileSizeMB).toBe(50)
    expect(s.parser.spacesPerIndent).toBe(4)
    expect(s.appearance.theme).toBe('dark')
    expect(s.appearance.density).toBe('comfortable')
    expect(s.appearance.codeFont).toBeTruthy()
    expect(s.appearance.uiFont).toBeTruthy()
    expect(s.ai.activeProvider).toBeTruthy()
    expect(s.ai.dailyBudget).toBeGreaterThan(0)
    expect(s.ai.demoMode.dailyLimit).toBeGreaterThan(0)
    expect(s.layout.sidebarWidth).toBeGreaterThan(0)
    expect(s.layout.inspectorWidth).toBeGreaterThan(0)
    expect(s.layout.lastActiveInspectorTab).toBeTruthy()
    expect(s.layout.lastViewMode).toBeTruthy()
    expect(s.general.checkForUpdates).toBeTruthy()
  })
})

// ---- updateAppearance ----

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

// ---- updateLayout ----

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

// ---- updateProvider ----

describe('updateProvider', () => {
  it('updates only the specified provider', async () => {
    const p = useSettingsStore.getState().updateProvider('openai', {enabled: true})
    await flushPersist()
    await p
    const ai = useSettingsStore.getState().settings.ai
    expect(ai.providers.openai.enabled).toBe(true)
    // Other providers should be unchanged
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
})

// ---- resetToDefaults ----

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

// ---- persist queue semantics (debounce / supersede / in-flight guard) ----
//
// These were previously impossible to test because the debounce timer,
// supersede resolver, and in-flight promise lived in unresettable module-level
// vars. __resetPersistQueueForTest now clears them between cases, so the queue
// contract is lockable.

describe('persist queue', () => {
  it('coalesces rapid updates into one backend write (debounce)', async () => {
    const store = useSettingsStore.getState()
    const p1 = store.updateAppearance({theme: 'nord'})
    const p2 = store.updateAppearance({theme: 'dracula'})
    const p3 = store.updateAppearance({theme: 'tokyo-night'})

    await flushPersist()
    await Promise.all([p1, p2, p3])

    // One debounced flush → one network write, carrying the latest value.
    expect(mockUpdate).toHaveBeenCalledOnce()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('tokyo-night')
  })

  it('resolves a superseded persist immediately rather than hanging', async () => {
    const store = useSettingsStore.getState()
    const superseded = store.updateAppearance({theme: 'nord'})

    // A second update supersedes the first; the first must settle (not hang).
    const winner = store.updateAppearance({theme: 'dracula'})

    await flushPersist()
    await Promise.all([superseded, winner])

    // Both settled without throwing — the supersede path resolved the first.
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('dracula')
  })

  it('queues a follow-up write behind an in-flight one', async () => {
    // Make the first write slow so a second persist arrives while it's airborne.
    let releaseFirst: () => void = () => {}
    mockUpdate.mockImplementationOnce(
      () =>
        new Promise<void>(resolve => {
          releaseFirst = resolve
        }),
    )

    const store = useSettingsStore.getState()
    const first = store.updateAppearance({theme: 'nord'})
    await flushPersist() // fires the debounced first write (now pending)

    // While the first is in flight, issue a second update.
    const second = store.updateAppearance({theme: 'dracula'})
    await flushPersist() // queues behind the in-flight guard

    // Nothing new has been written yet — the in-flight guard holds it.
    expect(mockUpdate).toHaveBeenCalledTimes(1)

    releaseFirst()
    await first
    await flushPersist() // second write now flushes
    await second

    expect(mockUpdate).toHaveBeenCalledTimes(2)
  })
})
