import {create} from 'zustand'
import type {FlowDiff} from '@/types/domain'

interface UiState {
  resolvedTheme: 'dark' | 'light' | 'midnight' | 'warm' | 'tokyo-night' | 'one-dark' | 'dracula' | 'nord'
  sidebarTab: 'explorer' | 'variables' | 'library'
  mainPaneView: 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin'
  inspectorTab: 'details' | 'ai' | 'findings' | 'metrics' | 'sharing'
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

  setResolvedTheme: (t: 'dark' | 'light' | 'midnight' | 'warm' | 'tokyo-night' | 'one-dark' | 'dracula' | 'nord') => void
  setSidebarTab: (t: 'explorer' | 'variables' | 'library') => void
  setMainPaneView: (v: 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin') => void
  setInspectorTab: (t: 'details' | 'ai' | 'findings' | 'metrics' | 'sharing') => void
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
  mainPaneView: 'block',
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
