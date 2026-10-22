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
    useUIStore.setState({settingsOpen: false, settingsSection: null, settingsAnchor: null, lastSettingsSection: null})
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

  it('carries a one-shot anchor with the deep link until the panel clears it', () => {
    useUIStore.getState().openSettings('accounts', 'provider-azure-foundry')
    expect(useUIStore.getState().settingsSection).toBe('accounts')
    expect(useUIStore.getState().settingsAnchor).toBe('provider-azure-foundry')
    useUIStore.getState().clearSettingsAnchor()
    expect(useUIStore.getState().settingsAnchor).toBeNull()
    expect(useUIStore.getState().settingsSection).toBe('accounts')
  })

  it('retargets an open modal and drops a stale anchor on a plain open', () => {
    useUIStore.getState().openSettings('accounts', 'provider-azure-foundry')
    useUIStore.getState().openSettings('orgs')
    expect(useUIStore.getState().settingsOpen).toBe(true)
    expect(useUIStore.getState().settingsSection).toBe('orgs')
    expect(useUIStore.getState().settingsAnchor).toBeNull()
  })

  it('closing by any route drops the anchor', async () => {
    useUIStore.getState().openSettings('accounts', 'provider-azure-foundry')
    useUIStore.getState().closeSettings()
    expect(useUIStore.getState().settingsAnchor).toBeNull()

    useUIStore.getState().openSettings('accounts', 'provider-azure-foundry')
    useUIStore.getState().toggleSettings()
    expect(useUIStore.getState().settingsAnchor).toBeNull()

    useUIStore.getState().openSettings('accounts', 'provider-azure-foundry')
    await resetAllStores()
    expect(useUIStore.getState().settingsAnchor).toBeNull()
  })

  describe('subflowMap layout preferences', () => {
    it('updates layout, direction, and routing', () => {
      expect(useUIStore.getState().subflowMapLayout).toBe('hierarchy')
      expect(useUIStore.getState().subflowMapDirection).toBe('LR')
      expect(useUIStore.getState().subflowMapRouting).toBe('corridor')

      useUIStore.getState().setSubflowMapLayout('radial')
      expect(useUIStore.getState().subflowMapLayout).toBe('radial')

      useUIStore.getState().setSubflowMapDirection('TB')
      expect(useUIStore.getState().subflowMapDirection).toBe('TB')

      useUIStore.getState().setSubflowMapRouting('straight')
      expect(useUIStore.getState().subflowMapRouting).toBe('straight')
    })

    it('resets map preferences to defaults on resetAllStores', async () => {
      useUIStore.getState().setSubflowMapLayout('cluster')
      useUIStore.getState().setSubflowMapDirection('TB')
      useUIStore.getState().setSubflowMapRouting('straight')

      await resetAllStores()

      expect(useUIStore.getState().subflowMapLayout).toBe('hierarchy')
      expect(useUIStore.getState().subflowMapDirection).toBe('LR')
      expect(useUIStore.getState().subflowMapRouting).toBe('corridor')
    })
  })
})
