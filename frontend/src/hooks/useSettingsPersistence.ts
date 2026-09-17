import {useEffect} from 'react'
import {useSettingsStore, onSettingsLoaded} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import type {ProviderID} from '@/types'

// canLoad: true once the backend will answer a settings read — the user is
// authenticated (cloud), or the app runs against the local sidecar (desktop,
// where there is no login and the auth gate never sets isAuthenticated).
export function useSettingsPersistence(canLoad: boolean) {
  const loadFromBackend = useSettingsStore(s => s.loadFromBackend)
  const updateLayout = useSettingsStore(s => s.updateLayout)

  useEffect(() => {
    // Settings live server-side; nothing to load until the backend will
    // answer. Guarded explicitly so the `canLoad` dependency is meaningful
    // (it re-triggers the load on login) rather than a no-op extra dep, and
    // we avoid a doomed backend fetch on the unauthenticated initial mount.
    if (!canLoad) return
    const unsub = onSettingsLoaded(s => {
      if (s.layout?.lastViewMode) useUIStore.getState().setMainPaneView(s.layout.lastViewMode)
      if (s.layout?.lastActiveInspectorTab) useUIStore.getState().setInspectorTab(s.layout.lastActiveInspectorTab)
      if (s.layout?.sidebarCollapsed !== undefined) useUIStore.getState().setSidebarCollapsed(s.layout.sidebarCollapsed)
      if (s.layout?.inspectorCollapsed !== undefined)
        useUIStore.getState().setInspectorCollapsed(s.layout.inspectorCollapsed)
      if (s.ai?.activeProvider) useChatStore.getState().setProvider(s.ai.activeProvider as ProviderID)
    })
    void loadFromBackend()
    return unsub
  }, [loadFromBackend, canLoad])

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

        void updateLayout({
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
