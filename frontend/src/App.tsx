import {useCallback, useEffect, useState, lazy, Suspense} from 'react'
import {ErrorBoundary, ToastProvider, useToast} from './components/shared'
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
import TitleBar from './components/layout/TitleBar'
import Sidebar from './components/layout/Sidebar'
import MainPane from './components/layout/MainPane'
import InspectorPanel from './components/layout/InspectorPanel'
import StatusBar from './components/layout/StatusBar'
import PaneDivider from './components/layout/PaneDivider'
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
    const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])

    const openDocument = useCallback((doc: DomainFlowDocument | null) => {
        setDocument(doc)
        useFlowStore.setState({ libraryFlowId: null, libraryVersion: 0 })
        if (doc && isSystemView(useUIStore.getState().mainPaneView)) {
            setMainPaneView('block')
        }
    }, [setDocument, setMainPaneView])

    useEffect(() => {
        useSystemStore.getState().loadInfo()
    }, [])

    useEffect(() => {
        let cancelled = false
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (!cancelled && files) setRecentFiles(files) })
            .catch((err) => { if (!cancelled) logger.warn('Failed to load recent files', err) })
        return () => { cancelled = true }
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

    useAppEvents({openDocument})

    const {toggleTheme} = useTheme()
    useAutoAnalyze()

    const showShortcuts = useCallback(() => setShortcutsHelpOpen(true), [])
    useTauriMenuEvents({openDocument, toggleTheme, onShowShortcuts: showShortcuts})

    const pane = usePaneResize()
    const {dragOver, handleDragOver, handleDragLeave, handleDrop} = useFileDrop(openDocument)

    useAppShortcuts({openDocument, toggleTheme, toast, setShortcutsHelpOpen})

    const commands = useCommandList({
        openDocument, toggleSidebar, toggleInspector, toggleSettings,
        setMainPaneView, requestSearchFocus, recentFiles,
        sidebarCollapsed, document, user, toast,
    })

    return (
        <>
        <div
            className={`flex flex-col h-screen w-screen overflow-hidden bg-surface-0 text-text-primary ${dragOver ? 'ring-2 ring-brand-500 ring-inset' : ''}`}
            onDragOver={handleDragOver}
            onDragLeave={handleDragLeave}
            onDrop={handleDrop}
        >
            <TitleBar />
            <div className="flex flex-1 overflow-hidden">
                {!sidebarCollapsed && (
                    <>
                        <div className="flex-shrink-0 overflow-hidden border-r border-border-subtle" style={{width: pane.sidebarWidth}}>
                            <ErrorBoundary fallbackMessage="Sidebar error">
                                <Sidebar />
                            </ErrorBoundary>
                        </div>
                        <PaneDivider onDrag={pane.handleSidebarDrag} onResizeEnd={pane.handleSidebarResizeEnd} onDoubleClick={pane.handleSidebarReset} />
                    </>
                )}
                <div className="flex-1 overflow-hidden">
                    <ErrorBoundary fallbackMessage="Main pane error">
                        <MainPane />
                    </ErrorBoundary>
                </div>
                {!inspectorCollapsed && (
                    <>
                        <PaneDivider onDrag={pane.handleInspectorDrag} onResizeEnd={pane.handleInspectorResizeEnd} onDoubleClick={pane.handleInspectorReset} />
                        <div className="flex-shrink-0 overflow-hidden border-l border-border-subtle" style={{width: pane.inspectorWidth}}>
                            <ErrorBoundary fallbackMessage="Inspector error">
                                <InspectorPanel />
                            </ErrorBoundary>
                        </div>
                    </>
                )}
            </div>
            <StatusBar />
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
        </>
    )
}

export default function App() {
    return (
        <ErrorBoundary>
            <ToastProvider>
                <AppInner />
            </ToastProvider>
        </ErrorBoundary>
    )
}
