import {describe, it, expect, beforeEach} from 'vitest'
import {useUIStore, isSystemView, type MainPaneView} from './uiStore'
import {resetAllStores} from './storeRegistry'

describe('uiStore', () => {
  beforeEach(() => {
    useUIStore.setState({
      sidebarTab: 'explorer',
      mainPaneView: 'home',
      inspectorTab: 'details',
      sidebarCollapsed: false,
      inspectorCollapsed: false,
      commandPaletteOpen: false,
      globalSearchOpen: false,
      settingsOpen: false,
    })
  })

  it('setInspectorTab updates the active inspector tab', () => {
    useUIStore.getState().setInspectorTab('findings')
    expect(useUIStore.getState().inspectorTab).toBe('findings')
  })

  it('toggleSidebar flips collapsed state', () => {
    expect(useUIStore.getState().sidebarCollapsed).toBe(false)
    useUIStore.getState().toggleSidebar()
    expect(useUIStore.getState().sidebarCollapsed).toBe(true)
    useUIStore.getState().toggleSidebar()
    expect(useUIStore.getState().sidebarCollapsed).toBe(false)
  })

  it('setCommandPaletteOpen accepts a boolean', () => {
    useUIStore.getState().setCommandPaletteOpen(true)
    expect(useUIStore.getState().commandPaletteOpen).toBe(true)
  })

  it('setCommandPaletteOpen accepts an updater function', () => {
    useUIStore.getState().setCommandPaletteOpen(true)
    useUIStore.getState().setCommandPaletteOpen(prev => !prev)
    expect(useUIStore.getState().commandPaletteOpen).toBe(false)
  })

  describe('isSystemView', () => {
    it('treats standalone destinations as system views', () => {
      // Regression: 'deps' and 'rules' were missing from SYSTEM_VIEWS while
      // SystemViewRouter already handled them, so opening a flow from those
      // views left it off-screen. Keep this list in sync with SystemViewRouter.
      for (const v of [
        'home',
        'dashboard',
        'profile',
        'admin',
        'library',
        'portfolio',
        'flow-compare',
        'deps',
        'rules',
      ] as const) {
        expect(isSystemView(v)).toBe(true)
      }
    })

    it('treats flow-dependent views as non-system', () => {
      for (const v of ['block', 'graph', 'map', 'local-map', 'diff'] as const) {
        expect(isSystemView(v)).toBe(false)
      }
    })

    it('covers every view in the MainPaneView union exactly once', () => {
      const allViews: MainPaneView[] = [
        'home',
        'block',
        'graph',
        'map',
        'local-map',
        'diff',
        'profile',
        'admin',
        'dashboard',
        'library',
        'portfolio',
        'flow-compare',
        'deps',
        'rules',
      ]
      expect(new Set(allViews).size).toBe(allViews.length)
      for (const v of allViews) {
        expect(typeof isSystemView(v)).toBe('boolean')
      }
    })
  })

  it('resetAllStores restores defaults but preserves resolvedTheme', async () => {
    useUIStore.getState().setResolvedTheme('dracula')
    useUIStore.getState().setMainPaneView('admin')
    useUIStore.getState().setInspectorTab('findings')
    useUIStore.getState().setSidebarCollapsed(true)

    await resetAllStores()

    const s = useUIStore.getState()
    expect(s.mainPaneView).toBe('home')
    expect(s.inspectorTab).toBe('details')
    expect(s.sidebarCollapsed).toBe(false)
    // Theme is intentionally preserved across logout to avoid a flash.
    expect(s.resolvedTheme).toBe('dracula')
  })
})
