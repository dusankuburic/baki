import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {FlowDiff} from '@/types'

export type MainPaneView =
  | 'home'
  | 'block'
  | 'graph'
  | 'map'
  | 'local-map'
  | 'diff'
  | 'profile'
  | 'admin'
  | 'dashboard'
  | 'library'
  | 'portfolio'
  | 'flow-compare'
  | 'deps'
  | 'rules'
export type ResolvedTheme =
  | 'dark'
  | 'light'
  | 'midnight'
  | 'warm'
  | 'tokyo-night'
  | 'one-dark'
  | 'dracula'
  | 'nord'
  | 'gruvbox-dark'
  | 'gruvbox-light'
  | 'catppuccin-mocha'
  | 'catppuccin-latte'
  | 'rose-pine'
  | 'rose-pine-moon'
  | 'rose-pine-dawn'
  | 'github-dark'
  | 'github-light'
  | 'kanagawa'
  | 'everforest'
export type SidebarTab = 'explorer' | 'variables' | 'library'
export type InspectorTab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing' | 'history'

export type SubflowMapLayout = 'hierarchy' | 'radial' | 'cluster'
export type SubflowMapDirection = 'LR' | 'TB'
export type SubflowMapRouting = 'corridor' | 'straight'

export type SettingsSection =
  | 'general'
  | 'parser'
  | 'accounts'
  | 'behavior'
  | 'prompts'
  | 'appearance'
  | 'analysis'
  | 'policies'
  | 'orgs'
  | 'knowledge'
  | 'tokens'
  | 'shortcuts'
  | 'privacy'
  | 'about'

export const SYSTEM_VIEWS: ReadonlySet<MainPaneView> = new Set([
  'home',
  'dashboard',
  'profile',
  'admin',
  'library',
  'portfolio',
  'flow-compare',
  'deps',
  'rules',
])
export const isSystemView = (v: MainPaneView): boolean => SYSTEM_VIEWS.has(v)

interface UiState {
  resolvedTheme: ResolvedTheme
  sidebarTab: SidebarTab
  mainPaneView: MainPaneView
  inspectorTab: InspectorTab
  sidebarCollapsed: boolean
  mobileDrawer: 'sidebar' | 'inspector' | null
  inspectorCollapsed: boolean
  commandPaletteOpen: boolean
  globalSearchOpen: boolean
  complexityMode: boolean
  settingsOpen: boolean
  settingsSection: SettingsSection | null
  settingsAnchor: string | null
  lastSettingsSection: SettingsSection | null
  variablePanelOpen: boolean
  selectedVariable: string | null
  graphZoom: number
  activeDiff: FlowDiff | null
  showSourceEditor: boolean
  sourceEditorDirty: boolean
  showHelpOverlay: boolean
  chatFocus: boolean
  subflowMapLayout: SubflowMapLayout
  subflowMapDirection: SubflowMapDirection
  subflowMapRouting: SubflowMapRouting

  setResolvedTheme: (t: ResolvedTheme) => void
  setSidebarTab: (t: SidebarTab) => void
  setMainPaneView: (v: MainPaneView) => void
  setInspectorTab: (t: InspectorTab) => void
  setSidebarCollapsed: (v: boolean) => void
  setInspectorCollapsed: (v: boolean) => void
  setCommandPaletteOpen: (o: boolean | ((prev: boolean) => boolean)) => void
  setGlobalSearchOpen: (o: boolean | ((prev: boolean) => boolean)) => void
  setSettingsOpen: (o: boolean) => void
  setSettingsSection: (s: SettingsSection | null) => void
  setLastSettingsSection: (s: SettingsSection) => void
  openSettings: (section?: SettingsSection, anchor?: string) => void
  clearSettingsAnchor: () => void
  closeSettings: () => void
  setVariablePanelOpen: (o: boolean) => void
  setSelectedVariable: (v: string | null) => void
  setGraphZoom: (z: number) => void
  setActiveDiff: (d: FlowDiff | null) => void
  setShowSourceEditor: (v: boolean) => void
  setSourceEditorDirty: (v: boolean) => void
  setShowHelpOverlay: (v: boolean) => void
  closeMobileDrawer: () => void
  toggleSidebar: () => void
  toggleComplexityMode: () => void
  toggleInspector: () => void
  toggleSettings: () => void
  setChatFocus: (on: boolean) => void
  toggleChatFocus: () => void
  setSubflowMapLayout: (layout: SubflowMapLayout) => void
  setSubflowMapDirection: (direction: SubflowMapDirection) => void
  setSubflowMapRouting: (routing: SubflowMapRouting) => void
}

function isDesktopViewport(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return true
  return window.matchMedia('(min-width: 768px)').matches
}

export const useUIStore = create<UiState>((set, get) => ({
  resolvedTheme: 'dark',
  sidebarTab: 'explorer',
  mainPaneView: 'home',
  inspectorTab: 'details',
  sidebarCollapsed: false,
  inspectorCollapsed: false,
  mobileDrawer: null,
  commandPaletteOpen: false,
  globalSearchOpen: false,
  complexityMode: false,
  settingsOpen: false,
  settingsSection: null,
  settingsAnchor: null,
  lastSettingsSection: null,
  variablePanelOpen: false,
  selectedVariable: null,
  graphZoom: 1,
  activeDiff: null,
  showSourceEditor: false,
  sourceEditorDirty: false,
  showHelpOverlay: false,
  chatFocus: false,
  subflowMapLayout: 'hierarchy',
  subflowMapDirection: 'LR',
  subflowMapRouting: 'corridor',

  setResolvedTheme: t => set({resolvedTheme: t}),
  setSidebarTab: t => set({sidebarTab: t}),
  setMainPaneView: v => set({mainPaneView: v}),
  setInspectorTab: t => set({inspectorTab: t}),
  setSidebarCollapsed: v => set({sidebarCollapsed: v}),
  setInspectorCollapsed: v => set({inspectorCollapsed: v}),
  setCommandPaletteOpen: o =>
    set(s => ({
      commandPaletteOpen: typeof o === 'function' ? o(s.commandPaletteOpen) : o,
    })),
  setGlobalSearchOpen: o =>
    set(s => ({
      globalSearchOpen: typeof o === 'function' ? o(s.globalSearchOpen) : o,
    })),
  setSettingsOpen: o => set({settingsOpen: o}),
  setSettingsSection: sec => set({settingsSection: sec}),
  setLastSettingsSection: sec => set({lastSettingsSection: sec}),
  openSettings: (section, anchor) =>
    set({settingsOpen: true, settingsSection: section ?? null, settingsAnchor: anchor ?? null}),
  clearSettingsAnchor: () => set({settingsAnchor: null}),
  closeSettings: () => set({settingsOpen: false, settingsSection: null, settingsAnchor: null}),
  setVariablePanelOpen: o => set({variablePanelOpen: o}),
  setSelectedVariable: v => set({selectedVariable: v}),
  setGraphZoom: z => set({graphZoom: z}),
  setActiveDiff: d => set({activeDiff: d}),
  setShowSourceEditor: v => set(v ? {showSourceEditor: true} : {showSourceEditor: false, sourceEditorDirty: false}),
  setSourceEditorDirty: v => set({sourceEditorDirty: v}),
  setShowHelpOverlay: v => set({showHelpOverlay: v}),
  closeMobileDrawer: () => set({mobileDrawer: null}),
  toggleSidebar: () =>
    set(s =>
      isDesktopViewport()
        ? {sidebarCollapsed: !s.sidebarCollapsed}
        : {mobileDrawer: s.mobileDrawer === 'sidebar' ? null : 'sidebar'},
    ),
  toggleComplexityMode: () => set(s => ({complexityMode: !s.complexityMode})),
  toggleInspector: () =>
    set(s =>
      s.chatFocus
        ? {chatFocus: false, inspectorTab: 'ai'}
        : isDesktopViewport()
          ? {inspectorCollapsed: !s.inspectorCollapsed}
          : {mobileDrawer: s.mobileDrawer === 'inspector' ? null : 'inspector'},
    ),
  toggleSettings: () =>
    set(s =>
      s.settingsOpen ? {settingsOpen: false, settingsSection: null, settingsAnchor: null} : {settingsOpen: true},
    ),
  setChatFocus: on =>
    set(s => {
      if (on) {
        if (!isDesktopViewport()) return {}
        return {chatFocus: true, inspectorTab: 'ai', inspectorCollapsed: false}
      }
      return s.chatFocus ? {chatFocus: false, inspectorTab: 'ai'} : {}
    }),
  toggleChatFocus: () => get().setChatFocus(!get().chatFocus),
  setSubflowMapLayout: layout => set({subflowMapLayout: layout}),
  setSubflowMapDirection: direction => set({subflowMapDirection: direction}),
  setSubflowMapRouting: routing => set({subflowMapRouting: routing}),
}))

registerStoreReset(() =>
  useUIStore.setState({
    sidebarTab: 'explorer',
    mainPaneView: 'home',
    inspectorTab: 'details',
    sidebarCollapsed: false,
    inspectorCollapsed: false,
    chatFocus: false,
    subflowMapLayout: 'hierarchy',
    subflowMapDirection: 'LR',
    subflowMapRouting: 'corridor',
    commandPaletteOpen: false,
    globalSearchOpen: false,
    complexityMode: false,
    settingsOpen: false,
    settingsSection: null,
    settingsAnchor: null,
    lastSettingsSection: null,
    variablePanelOpen: false,
    selectedVariable: null,
    graphZoom: 1,
    activeDiff: null,
    showSourceEditor: false,
    sourceEditorDirty: false,
    showHelpOverlay: false,
  }),
)
