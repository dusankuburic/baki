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

describe('uiStore chat focus mode', () => {
  beforeEach(() => {
    useUIStore.setState({chatFocus: false, inspectorTab: 'details', inspectorCollapsed: true})
  })

  it('entering shows the AI tab and un-collapses the inspector', () => {
    useUIStore.getState().setChatFocus(true)
    const s = useUIStore.getState()
    expect(s.chatFocus).toBe(true)
    expect(s.inspectorTab).toBe('ai')
    expect(s.inspectorCollapsed).toBe(false)
  })

  it('leaving lands back on the AI tab', () => {
    useUIStore.getState().setChatFocus(true)
    useUIStore.getState().setInspectorTab('findings')
    useUIStore.getState().toggleChatFocus()
    expect(useUIStore.getState().chatFocus).toBe(false)
    expect(useUIStore.getState().inspectorTab).toBe('ai')
  })

  it('toggleInspector while focused only leaves focus mode', () => {
    useUIStore.getState().setChatFocus(true)
    useUIStore.getState().toggleInspector()
    const s = useUIStore.getState()
    expect(s.chatFocus).toBe(false)
    expect(s.inspectorCollapsed).toBe(false)
    useUIStore.getState().toggleInspector()
    expect(useUIStore.getState().inspectorCollapsed).toBe(true)
  })

  it('resetAllStores clears focus mode', async () => {
    useUIStore.getState().setChatFocus(true)
    await resetAllStores()
    expect(useUIStore.getState().chatFocus).toBe(false)
  })
})

describe('uiStore settings modal', () => {
  beforeEach(() => {
    useUIStore.setState({settingsOpen: false, settingsSection: null, lastSettingsSection: null})
  })

  it('openSettings deep-links a section and closeSettings drops it', () => {
    useUIStore.getState().openSettings('accounts')
    expect(useUIStore.getState().settingsOpen).toBe(true)
    expect(useUIStore.getState().settingsSection).toBe('accounts')
    useUIStore.getState().closeSettings()
    expect(useUIStore.getState().settingsOpen).toBe(false)
    expect(useUIStore.getState().settingsSection).toBeNull()
  })

  it('opens without a section when none is given', () => {
    useUIStore.getState().openSettings()
    expect(useUIStore.getState().settingsOpen).toBe(true)
    expect(useUIStore.getState().settingsSection).toBeNull()
  })

  // Regression: Ctrl+, closes through the toggle, which used to leave the
  // consumed deep link behind, so the next plain open landed on it again.
  it('toggleSettings clears a consumed deep link when it closes', () => {
    useUIStore.getState().openSettings('orgs')
    useUIStore.getState().toggleSettings()
    expect(useUIStore.getState().settingsOpen).toBe(false)
    expect(useUIStore.getState().settingsSection).toBeNull()
    useUIStore.getState().toggleSettings()
    expect(useUIStore.getState().settingsOpen).toBe(true)
    expect(useUIStore.getState().settingsSection).toBeNull()
  })

  it('remembers the last section until the stores reset', async () => {
    useUIStore.getState().setLastSettingsSection('shortcuts')
    expect(useUIStore.getState().lastSettingsSection).toBe('shortcuts')
    await resetAllStores()
    expect(useUIStore.getState().lastSettingsSection).toBeNull()
  })
})
