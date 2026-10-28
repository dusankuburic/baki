import {useEffect} from 'react'
import {useSettingsStore, onSettingsLoaded} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {useAuthStore} from '@/stores/authStore'
import {isTauri} from '@/platform/guards'
import {useSettingsSaveToast} from './useSettingsSaveToast'

export function useSettingsPersistence(canLoad: boolean) {
  const loadFromBackend = useSettingsStore(s => s.loadFromBackend)
  const updateLayout = useSettingsStore(s => s.updateLayout)
  useSettingsSaveToast()

  useEffect(() => {
    if (!canLoad) return
    let restored = false
    const unsub = onSettingsLoaded(s => {
      if (restored) return
      restored = true
      if (s.layout?.lastViewMode) useUIStore.getState().setMainPaneView(s.layout.lastViewMode)
      if (s.layout?.lastActiveInspectorTab) useUIStore.getState().setInspectorTab(s.layout.lastActiveInspectorTab)
      if (s.layout?.sidebarCollapsed !== undefined) useUIStore.getState().setSidebarCollapsed(s.layout.sidebarCollapsed)
      if (s.layout?.inspectorCollapsed !== undefined)
        useUIStore.getState().setInspectorCollapsed(s.layout.inspectorCollapsed)
    })
    void loadFromBackend()
    return unsub
  }, [loadFromBackend, canLoad])

  // The stores reset on sign-out move the panes back to home: no layout write is made for that, or for anything
  // else while signed out (it would be refused and reported on the login screen). Sign-out clears the session
  // before it resets the stores, so the check is made when the panes move.
  useEffect(() => {
    const unsub = useUIStore.subscribe((state, prev) => {
      if (!isTauri() && !useAuthStore.getState().isAuthenticated) return
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
