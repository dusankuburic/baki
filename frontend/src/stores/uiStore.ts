import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {FlowDiff} from '@/types'

export type MainPaneView = 'home' | 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin' | 'dashboard' | 'library' | 'portfolio'
export type ResolvedTheme = 'dark' | 'light' | 'midnight' | 'warm' | 'tokyo-night' | 'one-dark' | 'dracula' | 'nord'
export type SidebarTab = 'explorer' | 'variables' | 'library'
export type InspectorTab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing' | 'history'

// "System" views are standalone destinations (don't depend on a loaded flow).
// Sidebar handlers use isSystemView to know when opening a flow should also
// transition the main pane back to a flow view ('block') so the new flow
// becomes visible; staying on a system view would leave the flow off-screen.
const SYSTEM_VIEWS: ReadonlySet<MainPaneView> = new Set(['home', 'dashboard', 'profile', 'admin', 'library', 'portfolio'])
export const isSystemView = (v: MainPaneView): boolean => SYSTEM_VIEWS.has(v)

interface UiState {
  resolvedTheme: ResolvedTheme
  sidebarTab: SidebarTab
  mainPaneView: MainPaneView
  inspectorTab: InspectorTab
  sidebarCollapsed: boolean
  inspectorCollapsed: boolean
  commandPaletteOpen: boolean
  globalSearchOpen: boolean
  complexityMode: boolean
  settingsOpen: boolean
  variablePanelOpen: boolean
  selectedVariable: string | null
  graphZoom: number
  activeDiff: FlowDiff | null

  setResolvedTheme: (t: ResolvedTheme) => void
  setSidebarTab: (t: SidebarTab) => void
  setMainPaneView: (v: MainPaneView) => void
  setInspectorTab: (t: InspectorTab) => void
  setSidebarCollapsed: (v: boolean) => void
  setInspectorCollapsed: (v: boolean) => void
  setCommandPaletteOpen: (o: boolean | ((prev: boolean) => boolean)) => void
  setGlobalSearchOpen: (o: boolean | ((prev: boolean) => boolean)) => void
  setSettingsOpen: (o: boolean) => void
  setVariablePanelOpen: (o: boolean) => void
  setSelectedVariable: (v: string | null) => void
  setGraphZoom: (z: number) => void
  setActiveDiff: (d: FlowDiff | null) => void
  toggleSidebar: () => void
  toggleComplexityMode: () => void
  toggleInspector: () => void
  toggleSettings: () => void
}

export const useUIStore = create<UiState>((set) => ({
  resolvedTheme: 'dark',
  sidebarTab: 'explorer',
  mainPaneView: 'home',
  inspectorTab: 'details',
  sidebarCollapsed: false,
  inspectorCollapsed: false,
  commandPaletteOpen: false,
  globalSearchOpen: false,
  complexityMode: false,
  settingsOpen: false,
  variablePanelOpen: false,
  selectedVariable: null,
  graphZoom: 1,
  activeDiff: null,

  setResolvedTheme: (t) => set({resolvedTheme: t}),
  setSidebarTab: (t) => set({sidebarTab: t}),
  setMainPaneView: (v) => set({mainPaneView: v}),
  setInspectorTab: (t) => set({inspectorTab: t}),
  setSidebarCollapsed: (v) => set({sidebarCollapsed: v}),
  setInspectorCollapsed: (v) => set({inspectorCollapsed: v}),
  setCommandPaletteOpen: (o) => set(s => ({
    commandPaletteOpen: typeof o === 'function' ? o(s.commandPaletteOpen) : o,
  })),
  setGlobalSearchOpen: (o) => set(s => ({
    globalSearchOpen: typeof o === 'function' ? o(s.globalSearchOpen) : o,
  })),
  setSettingsOpen: (o) => set({settingsOpen: o}),
  setVariablePanelOpen: (o) => set({variablePanelOpen: o}),
  setSelectedVariable: (v) => set({selectedVariable: v}),
  setGraphZoom: (z) => set({graphZoom: z}),
  setActiveDiff: (d) => set({activeDiff: d}),
  toggleSidebar: () => set(s => ({sidebarCollapsed: !s.sidebarCollapsed})),
  toggleComplexityMode: () => set(s => ({complexityMode: !s.complexityMode})),
  toggleInspector: () => set(s => ({inspectorCollapsed: !s.inspectorCollapsed})),
  toggleSettings: () => set(s => ({settingsOpen: !s.settingsOpen})),
}))

// Reset on logout (see storeRegistry). resolvedTheme is intentionally preserved
// so the login screen doesn't flash before settings reload.
registerStoreReset(() => useUIStore.setState({
  sidebarTab: 'explorer', mainPaneView: 'home', inspectorTab: 'details',
  sidebarCollapsed: false, inspectorCollapsed: false, commandPaletteOpen: false,
  globalSearchOpen: false, complexityMode: false, settingsOpen: false,
  variablePanelOpen: false, selectedVariable: null, graphZoom: 1, activeDiff: null,
}))
