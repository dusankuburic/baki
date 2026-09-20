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

// Settings modal section ids. Typed here (not in SettingsModal) so deep-links
// like setSettingsSection('accounts') are checked at compile time — a typo
// ('providers') used to land silently on General.
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

// "System" views are standalone destinations (don't depend on a loaded flow).
// Sidebar handlers use isSystemView to know when opening a flow should also
// transition the main pane back to a flow view ('block') so the new flow
// becomes visible; staying on a system view would leave the flow off-screen.
//
// SINGLE SOURCE OF TRUTH: this set must stay in sync with the views handled
// by SystemViewRouter (which re-uses this predicate). Adding a view to the
// router's switch without adding it here reintroduces the "flow loads but
// stays off-screen" bug (it existed for 'deps'/'rules').
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
  // mobileDrawer is which overlay drawer is open below the md breakpoint.
  // It is deliberately SEPARATE from the collapsed flags above, which are the
  // persisted desktop layout: driving the drawers from those opened both of
  // them over the app on every first load at phone width (their default is
  // "pane shown"), and closing them would have overwritten the user's saved
  // desktop layout.
  mobileDrawer: 'sidebar' | 'inspector' | null
  inspectorCollapsed: boolean
  commandPaletteOpen: boolean
  globalSearchOpen: boolean
  complexityMode: boolean
  settingsOpen: boolean
  // Section the settings modal opens on (deep-links: "Set one up" from the
  // welcome tour, ApiKeyMissingState, etc.). Consumed on open, then reset.
  settingsSection: SettingsSection | null
  variablePanelOpen: boolean
  selectedVariable: string | null
  graphZoom: number
  activeDiff: FlowDiff | null
  showSourceEditor: boolean
  showHelpOverlay: boolean

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
  setVariablePanelOpen: (o: boolean) => void
  setSelectedVariable: (v: string | null) => void
  setGraphZoom: (z: number) => void
  setActiveDiff: (d: FlowDiff | null) => void
  setShowSourceEditor: (v: boolean) => void
  setShowHelpOverlay: (v: boolean) => void
  closeMobileDrawer: () => void
  toggleSidebar: () => void
  toggleComplexityMode: () => void
  toggleInspector: () => void
  toggleSettings: () => void
}

// isDesktopViewport mirrors useIsDesktop's breakpoint for the store's own
// decisions. Treats a non-browser environment as desktop so the pane flags
// behave as before under jsdom and SSR.
function isDesktopViewport(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return true
  return window.matchMedia('(min-width: 768px)').matches
}

export const useUIStore = create<UiState>(set => ({
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
  variablePanelOpen: false,
  selectedVariable: null,
  graphZoom: 1,
  activeDiff: null,
  showSourceEditor: false,
  showHelpOverlay: false,

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
  setVariablePanelOpen: o => set({variablePanelOpen: o}),
  setSelectedVariable: v => set({selectedVariable: v}),
  setGraphZoom: z => set({graphZoom: z}),
  setActiveDiff: d => set({activeDiff: d}),
  setShowSourceEditor: v => set({showSourceEditor: v}),
  setShowHelpOverlay: v => set({showHelpOverlay: v}),
  closeMobileDrawer: () => set({mobileDrawer: null}),
  // Below md the same control opens and closes the overlay drawer instead of
  // collapsing a pane, so every existing caller (title bar, command palette,
  // shortcuts) keeps working on both layouts.
  toggleSidebar: () =>
    set(s =>
      isDesktopViewport()
        ? {sidebarCollapsed: !s.sidebarCollapsed}
        : {mobileDrawer: s.mobileDrawer === 'sidebar' ? null : 'sidebar'},
    ),
  toggleComplexityMode: () => set(s => ({complexityMode: !s.complexityMode})),
  toggleInspector: () =>
    set(s =>
      isDesktopViewport()
        ? {inspectorCollapsed: !s.inspectorCollapsed}
        : {mobileDrawer: s.mobileDrawer === 'inspector' ? null : 'inspector'},
    ),
  toggleSettings: () => set(s => ({settingsOpen: !s.settingsOpen})),
}))

// Reset on logout (see storeRegistry). resolvedTheme is intentionally preserved
// so the login screen doesn't flash before settings reload.
registerStoreReset(() =>
  useUIStore.setState({
    sidebarTab: 'explorer',
    mainPaneView: 'home',
    inspectorTab: 'details',
    sidebarCollapsed: false,
    inspectorCollapsed: false,
    commandPaletteOpen: false,
    globalSearchOpen: false,
    complexityMode: false,
    settingsOpen: false,
    settingsSection: null,
    variablePanelOpen: false,
    selectedVariable: null,
    graphZoom: 1,
    activeDiff: null,
    showSourceEditor: false,
    showHelpOverlay: false,
  }),
)
