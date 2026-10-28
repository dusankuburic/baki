import {describe, it, expect, beforeEach, vi} from 'vitest'
import {act, renderHook} from '@testing-library/react'
import {useSettingsPersistence} from './useSettingsPersistence'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {useAuthStore} from '@/stores/authStore'

const updateLayout = vi.fn().mockResolvedValue(undefined)

beforeEach(() => {
  updateLayout.mockClear()
  useSettingsStore.setState({updateLayout, loadFromBackend: vi.fn().mockResolvedValue(undefined)})
  useUIStore.setState({mainPaneView: 'block'})
})

// Review of R-frontend-10: sign-out resets the panes to home after it clears the session; that move, and any other
// while signed out, makes no layout write (it would be refused and reported on the login screen).
describe('useSettingsPersistence layout writes', () => {
  it('saves a pane move while signed in, and none while signed out', () => {
    useAuthStore.setState({isAuthenticated: true})
    renderHook(() => useSettingsPersistence(false))
    act(() => useUIStore.getState().setMainPaneView('graph'))
    expect(updateLayout).toHaveBeenCalledOnce()

    useAuthStore.setState({isAuthenticated: false})
    act(() => useUIStore.getState().setMainPaneView('home'))
    expect(updateLayout).toHaveBeenCalledOnce()
  })
})
