import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useSettingsStore } from './settingsStore'

// Stub out the API layer so tests don't need a real backend
vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

import { settingsApi } from '@/api'

const mockGet = settingsApi.getSettings as ReturnType<typeof vi.fn>
const mockUpdate = settingsApi.updateSettings as ReturnType<typeof vi.fn>

// Snapshot of the store in its initial state — captured before any test mutates it.
// Zustand keeps plain object state; using `setState(snapshot, true)` fully restores it.
const initialState = useSettingsStore.getState()

beforeEach(() => {
  // Full replacement so no settings bleed from previous test
  useSettingsStore.setState(initialState, true)
  // Reset mock call history AND implementation back to defaults
  vi.resetAllMocks()
  mockGet.mockResolvedValue(null)
  mockUpdate.mockResolvedValue(undefined)
})

// ---- loadFromBackend ----

describe('loadFromBackend', () => {
  it('sets isLoaded to true on success', async () => {
    mockGet.mockResolvedValue({ ...initialState.settings, appearance: { theme: 'light' } })

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().isLoaded).toBe(true)
  })

  it('merges loaded settings into state', async () => {
    mockGet.mockResolvedValue({ ...initialState.settings, appearance: { theme: 'light' } })

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('light')
  })

  it('sets isLoaded even if the API throws', async () => {
    mockGet.mockRejectedValue(new Error('network'))

    await useSettingsStore.getState().loadFromBackend()
    expect(useSettingsStore.getState().isLoaded).toBe(true)
  })
})

// ---- updateAppearance ----

describe('updateAppearance', () => {
  it('updates the appearance theme optimistically', async () => {
    await useSettingsStore.getState().updateAppearance({ theme: 'midnight' })
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('midnight')
  })

  it('persists the new settings to the backend', async () => {
    await useSettingsStore.getState().updateAppearance({ theme: 'nord' })
    expect(mockUpdate).toHaveBeenCalledOnce()
  })

  it('rolls back to the previous state when persist fails', async () => {
    const originalTheme = useSettingsStore.getState().settings.appearance.theme
    mockUpdate.mockRejectedValue(new Error('disk full'))

    await useSettingsStore.getState().updateAppearance({ theme: 'dracula' })
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(originalTheme)
  })
})

// ---- updateLayout ----

describe('updateLayout', () => {
  it('updates sidebar width', async () => {
    await useSettingsStore.getState().updateLayout({ sidebarWidth: 350 })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(350)
  })

  it('does not touch other layout fields', async () => {
    const orig = useSettingsStore.getState().settings.layout.inspectorWidth
    await useSettingsStore.getState().updateLayout({ sidebarWidth: 400 })
    expect(useSettingsStore.getState().settings.layout.inspectorWidth).toBe(orig)
  })
})

// ---- updateProvider ----

describe('updateProvider', () => {
  it('updates only the specified provider', async () => {
    await useSettingsStore.getState().updateProvider('openai', { enabled: true })
    const ai = useSettingsStore.getState().settings.ai
    expect(ai.providers.openai.enabled).toBe(true)
    // Other providers should be unchanged
    expect(ai.providers.claude.enabled).toBe(true)
  })

  it('merges provider config rather than replacing it', async () => {
    const origTokens = useSettingsStore.getState().settings.ai.providers.claude.maxTokens
    await useSettingsStore.getState().updateProvider('claude', { temperature: 0.9 })
    expect(useSettingsStore.getState().settings.ai.providers.claude.temperature).toBe(0.9)
    expect(useSettingsStore.getState().settings.ai.providers.claude.maxTokens).toBe(origTokens)
  })
})

// ---- resetToDefaults ----

describe('resetToDefaults', () => {
  it('restores the default theme', async () => {
    const defaultTheme = initialState.settings.appearance.theme
    await useSettingsStore.getState().updateAppearance({ theme: 'tokyo-night' })
    await useSettingsStore.getState().resetToDefaults()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe(defaultTheme)
  })

  it('calls persist with default settings', async () => {
    await useSettingsStore.getState().resetToDefaults()
    expect(mockUpdate).toHaveBeenCalledOnce()
  })
})
