import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'

const getSettings = vi.fn()
const updateSettings = vi.fn()

vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: (...a: unknown[]) => getSettings(...a),
    updateSettings: (...a: unknown[]) => updateSettings(...a),
  },
}))

import {useSettingsPersistence} from './useSettingsPersistence'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'

beforeEach(() => {
  vi.clearAllMocks()
  updateSettings.mockResolvedValue(undefined)
})

// The loader now also runs whenever settings:changed arrives — including the
// echo of this tab's own layout save. Re-running the start-up restore on those
// reloads yanked the user back to the saved view (out of the dashboard, which
// is never saved as lastViewMode) every time anything was saved.
describe('useSettingsPersistence', () => {
  it('restores the saved view on the first load only', async () => {
    getSettings.mockResolvedValue({layout: {lastViewMode: 'graph'}})
    renderHook(() => useSettingsPersistence(true))
    await act(async () => {
      await new Promise(r => setTimeout(r, 0))
    })
    expect(useUIStore.getState().mainPaneView).toBe('graph')

    act(() => useUIStore.getState().setMainPaneView('dashboard'))
    await act(async () => {
      await useSettingsStore.getState().loadFromBackend()
    })
    expect(useUIStore.getState().mainPaneView).toBe('dashboard')
  })
})
