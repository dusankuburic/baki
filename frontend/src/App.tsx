import {useCallback, useEffect, useState, lazy, Suspense} from 'react'
import {ErrorBoundary, ToastProvider, ConfirmProvider, useToast} from './components/shared'
import SyncDropNotifier from './components/shared/SyncDropNotifier'
import OfflineIndicator from './components/shared/OfflineIndicator'
import CommandPalette from './components/search/CommandPalette'
import GlobalSearchOverlay from './components/search/GlobalSearchOverlay'
import ShortcutsHelpDialog from './components/search/ShortcutsHelpDialog'

const SettingsModal = lazy(() => import('./components/settings/SettingsModal'))
import {useUIStore, isSystemView} from './stores/uiStore'
import {useSystemStore} from './stores/systemStore'
import {logger} from './lib/logger'
import {useAuthStore} from './stores/authStore'
import {useFlowStore} from './stores/flowStore'
import {useSearchStore} from './stores/searchStore'
import {usePresenceStore} from './stores/presenceStore'
import {startGovernancePolling, stopGovernancePolling} from './stores/governanceStore'
import {useTheme} from './hooks/useTheme'
import {useTauriMenuEvents} from './hooks/useTauriMenuEvents'
import {useAutoAnalyze} from './hooks/useAutoAnalyze'
import {useAppShortcuts} from './hooks/useAppShortcuts'
import {useCommandList} from './hooks/useCommandList'
import {useAppEvents} from './hooks/useAppEvents'
import {usePaneResize} from './hooks/usePaneResize'
import {useFileDrop} from './hooks/useFileDrop'
import {useGlobalErrorHandler} from './hooks/useGlobalErrorHandler'
import {useSettingsPersistence} from './hooks/useSettingsPersistence'
import {useFlowChangeSync} from './hooks/useFlowChangeSync'
import {useSettingsStore} from './stores/settingsStore'
import {useIsDesktop} from './hooks/useMediaQuery'
import TitleBar from './components/layout/TitleBar'
import Sidebar from './components/layout/Sidebar'
import MainPane from './components/layout/MainPane'
import InspectorPanel from './components/layout/InspectorPanel'
import MobileDrawer from './components/layout/MobileDrawer'
import StatusBar from './components/layout/StatusBar'
import PaneDivider from './components/layout/PaneDivider'
import WelcomeModal from './components/onboarding/WelcomeModal'
import {flowApi} from '@/api'
import {isTauri} from '@/platform/guards'
import type {FlowDocument as DomainFlowDocument, RecentFile} from './types'

function AppInner() {
  const toast = useToast()
  const sidebarCollapsed = useUIStore(s => s.sidebarCollapsed)
  const inspectorCollapsed = useUIStore(s => s.inspectorCollapsed)
  const setCommandPaletteOpen = useUIStore(s => s.setCommandPaletteOpen)
  const globalSearchOpen = useUIStore(s => s.globalSearchOpen)
  const setGlobalSearchOpen = useUIStore(s => s.setGlobalSearchOpen)
  const toggleSidebar = useUIStore(s => s.toggleSidebar)
  const toggleInspector = useUIStore(s => s.toggleInspector)
  const toggleSettings = useUIStore(s => s.toggleSettings)
  const commandPaletteOpen = useUIStore(s => s.commandPaletteOpen)
  const settingsOpen = useUIStore(s => s.settingsOpen)
  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const document = useFlowStore(s => s.document)
  const user = useAuthStore(s => s.user)
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const requestSearchFocus = useSearchStore(s => s.requestFocus)
  const [shortcutsHelpOpen, setShortcutsHelpOpen] = useState(false)

  // Responsive: below md (768px) the 3-pane shell collapses — sidebar and
  // inspector become overlay drawers; main pane takes full width.
  const isDesktop = useIsDesktop()

  // Onboarding: show the welcome tour only on first run (firstRunCompleted
  // is false in the default settings) — and only once settings have loaded
  // from the backend so we don't flash it to returning users.
  const firstRunCompleted = useSettingsStore(s => s.settings.general.firstRunCompleted)
  const settingsLoaded = useSettingsStore(s => s.isLoaded)
  const [welcomeDismissed, setWelcomeDismissed] = useState(false)
  const showWelcome = settingsLoaded && !firstRunCompleted && !welcomeDismissed
  const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])

  const openDocument = useCallback(
    (doc: DomainFlowDocument | null) => {
      setDocument(doc)
      useFlowStore.setState({libraryFlowId: null, libraryVersion: 0})
      if (doc && isSystemView(useUIStore.getState().mainPaneView)) {
        setMainPaneView('block')
      }
    },
    [setDocument, setMainPaneView],
  )

  useEffect(() => {
    useSystemStore.getState().loadInfo()
  }, [])

  useEffect(() => {
    let cancelled = false
    flowApi
      .recentFiles()
      .then((files: RecentFile[]) => {
        if (!cancelled && files) setRecentFiles(files)
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load recent files', err)
      })
    return () => {
      cancelled = true
    }
  }, [document?.id])

  const documentId = document?.id ?? null
  useEffect(() => {
    if (isTauri() || !documentId) return
    usePresenceStore.getState().connectToFlow(documentId)
    return () => usePresenceStore.getState().disconnect()
  }, [documentId])

  useSettingsPersistence(isAuthenticated)
  useGlobalErrorHandler()
  useFlowChangeSync()

  // Governance-alerts bell: poll the unread count while authenticated, stop on
  // logout (the store reset also stops polling as a safety net).
  useEffect(() => {
    if (!isAuthenticated) {
      stopGovernancePolling()
      return
    }
    startGovernancePolling()
    return () => stopGovernancePolling()
  }, [isAuthenticated])

  useAppEvents({openDocument})

  const {toggleTheme} = useTheme()
  useAutoAnalyze()

  const showShortcuts = useCallback(() => setShortcutsHelpOpen(true), [])
  useTauriMenuEvents({openDocument, toggleTheme, onShowShortcuts: showShortcuts})

  const pane = usePaneResize()
  const {dragOver, handleDragOver, handleDragLeave, handleDrop} = useFileDrop(openDocument)

  useAppShortcuts({openDocument, toggleTheme, toast, setShortcutsHelpOpen})

  const commands = useCommandList({
    openDocument,
    toggleSidebar,
    toggleInspector,
    toggleSettings,
    setMainPaneView,
    requestSearchFocus,
    recentFiles,
    sidebarCollapsed,
    document,
    user,
    toast,
  })

  return (
    <>
      {/* Skip-to-content link: first focusable element, lets keyboard / SR users
        jump straight to the flow view past the sidebar/inspector chrome. */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-[100] focus:px-3 focus:py-1.5 focus:rounded focus:bg-brand-600 focus:text-brand-foreground focus:text-sm focus:shadow-lg"
      >
        Skip to content
      </a>
      <div
        className={`flex flex-col h-screen w-screen overflow-hidden bg-surface-0 text-text-primary ${dragOver ? 'ring-2 ring-brand-500 ring-inset' : ''}`}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      >
        <TitleBar />
        <div className="flex flex-1 overflow-hidden print:overflow-visible">
          {/* Desktop sidebar (inline pane) */}
          {isDesktop && !sidebarCollapsed && (
            <>
              <div
                className="flex-shrink-0 overflow-hidden border-r border-border-subtle print:hidden"
                style={{width: pane.sidebarWidth}}
                role="navigation"
                aria-label="Flow library and navigation"
              >
                <ErrorBoundary fallbackMessage="Sidebar error">
                  <Sidebar />
                </ErrorBoundary>
              </div>
              <PaneDivider
                onDrag={pane.handleSidebarDrag}
                onResizeEnd={pane.handleSidebarResizeEnd}
                onDoubleClick={pane.handleSidebarReset}
              />
            </>
          )}
          <div
            id="main-content"
            className="flex-1 overflow-hidden print:overflow-visible focus:outline-none"
            role="main"
            tabIndex={-1}
          >
            <ErrorBoundary fallbackMessage="Main pane error">
              <MainPane />
            </ErrorBoundary>
          </div>
          {/* Desktop inspector (inline pane) */}
          {isDesktop && !inspectorCollapsed && (
            <>
              <PaneDivider
                onDrag={pane.handleInspectorDrag}
                onResizeEnd={pane.handleInspectorResizeEnd}
                onDoubleClick={pane.handleInspectorReset}
              />
              <div
                className="flex-shrink-0 overflow-hidden border-l border-border-subtle print:hidden"
                style={{width: pane.inspectorWidth}}
                role="complementary"
                aria-label="Inspector"
              >
                <ErrorBoundary fallbackMessage="Inspector error">
                  <InspectorPanel />
                </ErrorBoundary>
              </div>
            </>
          )}
        </div>
        {/* Mobile sidebar drawer (overlay) */}
        {!isDesktop && !sidebarCollapsed && (
          <MobileDrawer side="left" label="Sidebar" onClose={toggleSidebar}>
            <ErrorBoundary fallbackMessage="Sidebar error">
              <Sidebar />
            </ErrorBoundary>
          </MobileDrawer>
        )}
        {/* Mobile inspector drawer (overlay, right-aligned) */}
        {!isDesktop && !inspectorCollapsed && (
          <MobileDrawer side="right" label="Inspector" onClose={toggleInspector}>
            <ErrorBoundary fallbackMessage="Inspector error">
              <InspectorPanel />
            </ErrorBoundary>
          </MobileDrawer>
        )}
        <StatusBar />
        <OfflineIndicator />
        {dragOver && (
          <div className="fixed inset-0 z-modal bg-surface-overlay flex items-center justify-center pointer-events-none">
            <div className="text-lg font-medium text-text-primary animate-fade-in">Drop flow file to open</div>
          </div>
        )}
      </div>
      <ErrorBoundary>
        <CommandPalette isOpen={commandPaletteOpen} onClose={() => setCommandPaletteOpen(false)} commands={commands} />
      </ErrorBoundary>
      <ErrorBoundary>
        <GlobalSearchOverlay isOpen={globalSearchOpen} onClose={() => setGlobalSearchOpen(false)} />
      </ErrorBoundary>
      <ErrorBoundary>
        <Suspense fallback={null}>
          <SettingsModal isOpen={settingsOpen} onClose={() => setSettingsOpen(false)} />
        </Suspense>
      </ErrorBoundary>
      <ErrorBoundary>
        <ShortcutsHelpDialog isOpen={shortcutsHelpOpen} onClose={() => setShortcutsHelpOpen(false)} />
      </ErrorBoundary>
      <ErrorBoundary>
        <WelcomeModal isOpen={showWelcome} onClose={() => setWelcomeDismissed(true)} />
      </ErrorBoundary>
    </>
  )
}

export default function App() {
  return (
    <ErrorBoundary>
      <ToastProvider>
        <ConfirmProvider>
          <AppInner />
          <SyncDropNotifier />
        </ConfirmProvider>
      </ToastProvider>
    </ErrorBoundary>
  )
}
