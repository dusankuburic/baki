import {useEffect} from 'react'
import {useSettingsStore, onSettingsLoaded} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import type {ProviderID} from '@/types'

export function useSettingsPersistence(isAuthenticated: boolean) {
  const loadFromBackend = useSettingsStore(s => s.loadFromBackend)
  const updateLayout = useSettingsStore(s => s.updateLayout)

  useEffect(() => {
    // Settings live server-side; nothing to load until the user is
    // authenticated. Guarded explicitly so the `isAuthenticated` dependency
    // is meaningful (it re-triggers the load on login) rather than a no-op
    // extra dep, and we avoid a doomed backend fetch on the unauthenticated
    // initial mount.
    if (!isAuthenticated) return
    const unsub = onSettingsLoaded(s => {
      if (s.layout?.lastViewMode) useUIStore.getState().setMainPaneView(s.layout.lastViewMode)
      if (s.layout?.lastActiveInspectorTab) useUIStore.getState().setInspectorTab(s.layout.lastActiveInspectorTab)
      if (s.layout?.sidebarCollapsed !== undefined) useUIStore.getState().setSidebarCollapsed(s.layout.sidebarCollapsed)
      if (s.layout?.inspectorCollapsed !== undefined)
        useUIStore.getState().setInspectorCollapsed(s.layout.inspectorCollapsed)
      if (s.ai?.activeProvider) useChatStore.getState().setProvider(s.ai.activeProvider as ProviderID)
    })
    loadFromBackend()
    return unsub
  }, [loadFromBackend, isAuthenticated])

  useEffect(() => {
    const unsub = useUIStore.subscribe((state, prev) => {
      if (
        state.sidebarCollapsed !== prev.sidebarCollapsed ||
        state.inspectorCollapsed !== prev.inspectorCollapsed ||
        state.mainPaneView !== prev.mainPaneView ||
        state.inspectorTab !== prev.inspectorTab
      ) {
        const lastViewMode = ['block', 'graph', 'map', 'local-map', 'diff'].includes(state.mainPaneView)
          ? (state.mainPaneView as 'block' | 'graph' | 'map' | 'local-map' | 'diff')
          : undefined

        const lastActiveInspectorTab = ['details', 'ai', 'findings'].includes(state.inspectorTab)
          ? (state.inspectorTab as 'details' | 'ai' | 'findings')
          : undefined

        updateLayout({
          sidebarCollapsed: state.sidebarCollapsed,
          inspectorCollapsed: state.inspectorCollapsed,
          lastViewMode,
          lastActiveInspectorTab,
        })
      }
    })
    return unsub
  }, [updateLayout])
}
