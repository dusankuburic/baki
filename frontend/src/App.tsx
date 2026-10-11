import {useCallback, useEffect, useState, lazy, Suspense} from 'react'
import {useTranslation} from 'react-i18next'
import {ErrorBoundary, ToastProvider, ConfirmProvider, useToast, Spinner} from './components/shared'
import SyncDropNotifier from './components/shared/SyncDropNotifier'
import OfflineIndicator from './components/shared/OfflineIndicator'
import ShortcutsHelpDialog from './components/search/ShortcutsHelpDialog'

const CommandPalette = lazy(() => import('./components/search/CommandPalette'))
const GlobalSearchOverlay = lazy(() => import('./components/search/GlobalSearchOverlay'))

const SettingsModal = lazy(() => import('./components/settings/SettingsModal'))
import {useUIStore, isSystemView, type MainPaneView} from './stores/uiStore'
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
import {usePaneResize, MIN_SIDEBAR, MAX_SIDEBAR, MIN_INSPECTOR} from './hooks/usePaneResize'
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
import BackendCrashBanner from './components/layout/BackendCrashBanner'
import ReloadBanner from './components/layout/ReloadBanner'
import ChatStreamProvider from './components/chat/ChatStreamProvider'
import {flowApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {useFileWatcher} from '@/hooks/useFileWatcher'
import type {FlowDocument as DomainFlowDocument, RecentFile} from './types'

function AppInner() {
  const toast = useToast()
  const sidebarCollapsed = useUIStore(s => s.sidebarCollapsed)
  const inspectorCollapsed = useUIStore(s => s.inspectorCollapsed)
  const mobileDrawer = useUIStore(s => s.mobileDrawer)
  const closeMobileDrawer = useUIStore(s => s.closeMobileDrawer)
  const setChatFocus = useUIStore(s => s.setChatFocus)
  const setCommandPaletteOpen = useUIStore(s => s.setCommandPaletteOpen)
  const globalSearchOpen = useUIStore(s => s.globalSearchOpen)
  const setGlobalSearchOpen = useUIStore(s => s.setGlobalSearchOpen)
  const toggleSidebar = useUIStore(s => s.toggleSidebar)
  const toggleInspector = useUIStore(s => s.toggleInspector)
  const toggleSettings = useUIStore(s => s.toggleSettings)
  const commandPaletteOpen = useUIStore(s => s.commandPaletteOpen)
  const settingsOpen = useUIStore(s => s.settingsOpen)
  const closeSettings = useUIStore(s => s.closeSettings)
  const {t} = useTranslation('shell')
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const documentId = useFlowStore(s => s.document?.id ?? null)
  const settingsSection = useUIStore(s => s.settingsSection)
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const requestSearchFocus = useSearchStore(s => s.requestFocus)
  const [shortcutsHelpOpen, setShortcutsHelpOpen] = useState(false)

  const isDesktop = useIsDesktop()
  useEffect(() => {
    if (isDesktop) closeMobileDrawer()
    else setChatFocus(false)
  }, [isDesktop, closeMobileDrawer, setChatFocus])

  const firstRunCompleted = useSettingsStore(s => s.settings.general.firstRunCompleted)
  const settingsLoaded = useSettingsStore(s => s.isLoaded)
  const [welcomeDismissed, setWelcomeDismissed] = useState(false)
  const showWelcome = settingsLoaded && !firstRunCompleted && !welcomeDismissed

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
    void useSystemStore.getState().loadInfo()
  }, [])

  useEffect(() => {
    if (isTauri() || !documentId) return
    void usePresenceStore.getState().connectToFlow(documentId)
    return () => usePresenceStore.getState().disconnect()
  }, [documentId])

  useSettingsPersistence(isAuthenticated || isTauri())
  useGlobalErrorHandler()
  useFlowChangeSync()

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

  const {
    sidebarRef,
    inspectorRef,
    sidebarWidth,
    inspectorWidth,
    handleSidebarDrag,
    handleSidebarResizeEnd,
    handleSidebarReset,
    handleInspectorDrag,
    handleInspectorResizeEnd,
    handleInspectorReset,
    inspectorMax,
    handleSidebarJump,
    handleInspectorJump,
  } = usePaneResize()
  const {dragOver, handleDragOver, handleDragLeave, handleDrop} = useFileDrop(openDocument)

  useAppShortcuts({openDocument, toggleTheme, toast, setShortcutsHelpOpen})

  return (
    <>
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-100 focus:px-3 focus:py-1.5 focus:rounded focus:bg-brand-600 focus:text-brand-foreground focus:text-sm focus:shadow-lg"
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
        <ReloadBanner />
        <div className="flex flex-1 overflow-hidden print:overflow-visible">
          {isDesktop && !sidebarCollapsed && (
            <>
              <div
                ref={sidebarRef}
                className="shrink-0 overflow-hidden border-r border-border-subtle print:hidden"
                style={{width: sidebarWidth}}
                role="navigation"
                aria-label="Flow library and navigation"
              >
                <ErrorBoundary fallbackMessage="Sidebar error">
                  <Sidebar />
                </ErrorBoundary>
              </div>
              <PaneDivider
                label={t('panes.resizeSidebar')}
                value={sidebarWidth}
                min={MIN_SIDEBAR}
                max={MAX_SIDEBAR}
                onDrag={handleSidebarDrag}
                onResizeEnd={handleSidebarResizeEnd}
                onDoubleClick={handleSidebarReset}
                onJump={handleSidebarJump}
              />
            </>
          )}
          <div
            id="main-content"
            className="flex-1 overflow-hidden print:overflow-visible focus:outline-hidden"
            role="main"
            tabIndex={-1}
          >
            <ErrorBoundary fallbackMessage="Main pane error">
              <MainPane />
            </ErrorBoundary>
          </div>
          {isDesktop && !inspectorCollapsed && (
            <>
              <PaneDivider
                label={t('panes.resizeInspector')}
                value={inspectorWidth}
                min={MIN_INSPECTOR}
                max={inspectorMax}
                onDrag={handleInspectorDrag}
                onResizeEnd={handleInspectorResizeEnd}
                onDoubleClick={handleInspectorReset}
                onJump={handleInspectorJump}
              />
              <div
                ref={inspectorRef}
                className="shrink-0 overflow-hidden border-l border-border-subtle print:hidden"
                style={{width: inspectorWidth}}
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
        {!isDesktop && mobileDrawer === 'sidebar' && (
          <MobileDrawer side="left" label="Sidebar" onClose={closeMobileDrawer}>
            <ErrorBoundary fallbackMessage="Sidebar error">
              <Sidebar />
            </ErrorBoundary>
          </MobileDrawer>
        )}
        {!isDesktop && mobileDrawer === 'inspector' && (
          <MobileDrawer side="right" label="Inspector" onClose={closeMobileDrawer}>
            <ErrorBoundary fallbackMessage="Inspector error">
              <InspectorPanel />
            </ErrorBoundary>
          </MobileDrawer>
        )}
        <StatusBar />
        <OfflineIndicator />
        {dragOver && (
          <div className="fixed inset-0 z-modal bg-surface-overlay flex items-center justify-center pointer-events-none">
            <div className="text-lg font-medium text-text-primary animate-fade-in">{t('dropOverlay')}</div>
          </div>
        )}
      </div>
      <ErrorBoundary>
        <Suspense fallback={null}>
          {commandPaletteOpen && (
            <CommandPaletteContainer
              isOpen
              onClose={() => setCommandPaletteOpen(false)}
              openDocument={openDocument}
              toggleSidebar={toggleSidebar}
              toggleInspector={toggleInspector}
              toggleSettings={toggleSettings}
              setMainPaneView={setMainPaneView}
              requestSearchFocus={requestSearchFocus}
              sidebarCollapsed={sidebarCollapsed}
              toast={toast}
            />
          )}
        </Suspense>
      </ErrorBoundary>
      <ErrorBoundary>
        <Suspense fallback={null}>
          {globalSearchOpen && <GlobalSearchOverlay isOpen onClose={() => setGlobalSearchOpen(false)} />}
        </Suspense>
      </ErrorBoundary>
      <ErrorBoundary>
        <Suspense
          fallback={
            <div className="fixed inset-0 z-modal flex items-center justify-center bg-surface-overlay">
              <Spinner size={24} />
            </div>
          }
        >
          {settingsOpen && <SettingsModal isOpen initialSection={settingsSection} onClose={closeSettings} />}
        </Suspense>
      </ErrorBoundary>
      <ErrorBoundary>
        <ShortcutsHelpDialog isOpen={shortcutsHelpOpen} onClose={() => setShortcutsHelpOpen(false)} />
      </ErrorBoundary>
      <ErrorBoundary>
        <WelcomeModal isOpen={showWelcome} onClose={() => setWelcomeDismissed(true)} />
      </ErrorBoundary>
      {isTauri() && (
        <ErrorBoundary>
          <BackendCrashBanner />
        </ErrorBoundary>
      )}
    </>
  )
}

function CommandPaletteContainer({
  isOpen,
  onClose,
  openDocument,
  toggleSidebar,
  toggleInspector,
  toggleSettings,
  setMainPaneView,
  requestSearchFocus,
  sidebarCollapsed,
  toast,
}: {
  isOpen: boolean
  onClose: () => void
  openDocument: (doc: DomainFlowDocument | null) => void
  toggleSidebar: () => void
  toggleInspector: () => void
  toggleSettings: () => void
  setMainPaneView: (view: MainPaneView) => void
  requestSearchFocus: () => void
  sidebarCollapsed: boolean
  toast: ReturnType<typeof useToast>
}) {
  const document = useFlowStore(s => s.document)
  const user = useAuthStore(s => s.user)
  const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])

  useFileWatcher(document)

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

  return <CommandPalette isOpen={isOpen} onClose={onClose} commands={commands} />
}

export default function App() {
  return (
    <ErrorBoundary>
      <ToastProvider>
        <ConfirmProvider>
          <ChatStreamProvider>
            <AppInner />
          </ChatStreamProvider>
          <SyncDropNotifier />
        </ConfirmProvider>
      </ToastProvider>
    </ErrorBoundary>
  )
}
