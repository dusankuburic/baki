import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, renderHook, act} from '@testing-library/react'

// Stub out the API layer so the settings store doesn't need a real backend.
vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

import {useTheme} from './useTheme'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {settingsApi} from '@/api'

const mockUpdate = settingsApi.updateSettings as ReturnType<typeof vi.fn>

const settingsInitialState = useSettingsStore.getState()
const uiInitialState = useUIStore.getState()

function Probe() {
  useTheme()
  return null
}

beforeEach(() => {
  vi.useFakeTimers()
  useSettingsStore.setState(settingsInitialState, true)
  useUIStore.setState(uiInitialState, true)
  vi.resetAllMocks()
  // resetAllMocks clears the factory implementations — re-establish them.
  mockUpdate.mockResolvedValue(undefined)
  document.documentElement.removeAttribute('data-theme')
  document.documentElement.removeAttribute('data-density')
  document.documentElement.removeAttribute('data-reduce-motion')
  document.documentElement.removeAttribute('data-high-contrast')
  try {
    localStorage.clear()
  } catch {
    /* jsdom guard */
  }
})

afterEach(() => {
  vi.useRealTimers()
})

async function flushPersist() {
  await vi.advanceTimersByTimeAsync(1000)
}

describe('useTheme', () => {
  it('reflects density on data-density', async () => {
    render(<Probe />)
    act(() => {
      void useSettingsStore.getState().updateAppearance({density: 'compact'})
    })
    await flushPersist()

    expect(document.documentElement.dataset.density).toBe('compact')
  })

  it('writes the resolved theme to data-theme and localStorage', async () => {
    render(<Probe />)
    act(() => {
      void useSettingsStore.getState().updateAppearance({theme: 'dracula'})
    })
    await flushPersist()

    expect(document.documentElement.dataset.theme).toBe('dracula')
    expect(localStorage.getItem('pad-theme')).toBe('dracula')
  })

  it('toggles data-reduce-motion between true and false', async () => {
    render(<Probe />)
    act(() => {
      void useSettingsStore.getState().updateAppearance({reduceMotion: true})
    })
    await flushPersist()
    expect(document.documentElement.dataset.reduceMotion).toBe('true')

    act(() => {
      void useSettingsStore.getState().updateAppearance({reduceMotion: false})
    })
    await flushPersist()
    expect(document.documentElement.dataset.reduceMotion).toBe('false')
  })

  it('toggles data-high-contrast between true and false', async () => {
    render(<Probe />)
    act(() => {
      void useSettingsStore.getState().updateAppearance({highContrast: true})
    })
    await flushPersist()
    expect(document.documentElement.dataset.highContrast).toBe('true')

    act(() => {
      void useSettingsStore.getState().updateAppearance({highContrast: false})
    })
    await flushPersist()
    expect(document.documentElement.dataset.highContrast).toBe('false')
  })

  it('toggleTheme flips dark→light and back', async () => {
    const {result} = renderHook(() => useTheme())

    // resolvedTheme defaults to 'dark' (uiStore initial)
    act(() => result.current.toggleTheme())
    await flushPersist()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('light')

    act(() => result.current.toggleTheme())
    await flushPersist()
    expect(useSettingsStore.getState().settings.appearance.theme).toBe('dark')
  })

  it('cross-fades a theme switch through a view transition, not the initial mount', () => {
    const startViewTransition = vi.fn((update: () => void) => {
      update()
      return {} as ViewTransition
    })
    Object.defineProperty(document, 'startViewTransition', {value: startViewTransition, configurable: true})
    try {
      render(<Probe />)
      expect(startViewTransition).not.toHaveBeenCalled()

      act(() => {
        void useSettingsStore.getState().updateAppearance({theme: 'nord'})
      })
      expect(startViewTransition).toHaveBeenCalledTimes(1)
      expect(document.documentElement.dataset.theme).toBe('nord')
      expect(useUIStore.getState().resolvedTheme).toBe('nord')
    } finally {
      delete (document as {startViewTransition?: unknown}).startViewTransition
    }
  })

  it('switches instantly, without a view transition, when motion is reduced', () => {
    const startViewTransition = vi.fn((update: () => void) => {
      update()
      return {} as ViewTransition
    })
    Object.defineProperty(document, 'startViewTransition', {value: startViewTransition, configurable: true})
    try {
      render(<Probe />)
      act(() => {
        void useSettingsStore.getState().updateAppearance({reduceMotion: true})
      })
      act(() => {
        void useSettingsStore.getState().updateAppearance({theme: 'nord'})
      })
      expect(startViewTransition).not.toHaveBeenCalled()
      expect(document.documentElement.dataset.theme).toBe('nord')
    } finally {
      delete (document as {startViewTransition?: unknown}).startViewTransition
    }
  })

  it('does not re-render its caller on a theme change', () => {
    let renders = 0
    function Counter() {
      useTheme()
      renders++
      return null
    }
    render(<Counter />)
    const before = renders
    act(() => {
      void useSettingsStore.getState().updateAppearance({theme: 'dracula'})
    })
    expect(renders).toBe(before)
    expect(document.documentElement.dataset.theme).toBe('dracula')
  })

  it('stores the preference ("system") not the resolved value in localStorage', async () => {
    // jsdom doesn't implement matchMedia — stub it so resolveTheme('system') works
    const mq = {matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn()}
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue(mq))

    render(<Probe />)
    act(() => {
      void useSettingsStore.getState().updateAppearance({theme: 'system'})
    })

    // localStorage stores the user's CHOICE ('system'), not the resolved OS value
    expect(localStorage.getItem('pad-theme')).toBe('system')

    vi.unstubAllGlobals()
  })
})
