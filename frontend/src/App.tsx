import {useCallback, useEffect, useRef, useState, lazy, Suspense} from 'react'
import {ErrorBoundary, ToastProvider, useToast} from './components/shared'
import CommandPalette from './components/search/CommandPalette'
import GlobalSearchOverlay from './components/search/GlobalSearchOverlay'
import ShortcutsHelpDialog from './components/search/ShortcutsHelpDialog'

const SettingsModal = lazy(() => import('./components/settings/SettingsModal'))
import {useSettingsStore, onSettingsLoaded} from './stores/settingsStore'
import {useUIStore} from './stores/uiStore'
import {logger} from './lib/logger'
import {useAuthStore} from './stores/authStore'
import {useFlowStore} from './stores/flowStore'
import {useChatStore} from './stores/chatStore'
import {useSearchStore} from './stores/searchStore'
import {usePresenceStore} from './stores/presenceStore'
import {useTheme} from './hooks/useTheme'
import {useTauriMenuEvents} from './hooks/useTauriMenuEvents'
import {useAutoAnalyze} from './hooks/useAutoAnalyze'
import {useAppShortcuts} from './hooks/useAppShortcuts'
import {useCommandList} from './hooks/useCommandList'
import {useAppEvents} from './hooks/useAppEvents'
import TitleBar from './components/layout/TitleBar'
import Sidebar from './components/layout/Sidebar'
import MainPane from './components/layout/MainPane'
import InspectorPanel from './components/layout/InspectorPanel'
import StatusBar from './components/layout/StatusBar'
import PaneDivider from './components/layout/PaneDivider'
import {flowApi, systemApi} from '@/api'
import {isTauri} from '@/platform/guards'
import type {FlowDocument as DomainFlowDocument, RecentFile, ProviderID} from './types/domain'

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 480
const MIN_INSPECTOR = 280
const MAX_INSPECTOR = 560

function AppInner() {
    const toast = useToast()
    const layout = useSettingsStore(s => s.settings.layout)
    const updateLayout = useSettingsStore(s => s.updateLayout)
    const loadFromBackend = useSettingsStore(s => s.loadFromBackend)
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
    const openDocument = useCallback((doc: DomainFlowDocument | null) => {
        setDocument(doc)
        if (doc && (useUIStore.getState().mainPaneView === 'profile' || useUIStore.getState().mainPaneView === 'admin')) {
            setMainPaneView('block')
        }
    }, [setDocument, setMainPaneView])

    const document = useFlowStore(s => s.document)
    const user = useAuthStore(s => s.user)
    const isAuthenticated = useAuthStore(s => s.isAuthenticated)
    const requestSearchFocus = useSearchStore(s => s.requestFocus)
    const [dragOver, setDragOver] = useState(false)
    const [shortcutsHelpOpen, setShortcutsHelpOpen] = useState(false)
    const dropRef = useRef<HTMLDivElement>(null)
    const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])
    const [sidebarLiveWidth, setSidebarLiveWidth] = useState<number | null>(null)
    const [inspectorLiveWidth, setInspectorLiveWidth] = useState<number | null>(null)
    const sidebarLiveWidthRef = useRef<number | null>(null)
    const inspectorLiveWidthRef = useRef<number | null>(null)
    // Stable ref so drag callbacks can read the current layout without closing over stale values
    const layoutRef = useRef(layout)
    useEffect(() => { layoutRef.current = layout })

    useEffect(() => {
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (files) setRecentFiles(files) })
            .catch((err) => { logger.warn('Failed to load recent files', err) })
    }, [document])

    // Real-time collaboration: join the presence channel for the open flow.
    // Only in web/cloud mode — desktop is single-user, so there is nobody to share with.
    const documentId = document?.id ?? null
    useEffect(() => {
        if (isTauri() || !documentId) return
        usePresenceStore.getState().connectToFlow(documentId)
        return () => usePresenceStore.getState().disconnect()
    }, [documentId])

    useEffect(() => {
        const unsub = onSettingsLoaded((s) => {
            if (s.layout?.lastViewMode) useUIStore.getState().setMainPaneView(s.layout.lastViewMode)
            if (s.layout?.lastActiveInspectorTab) useUIStore.getState().setInspectorTab(s.layout.lastActiveInspectorTab)
            if (s.layout?.sidebarCollapsed !== undefined) useUIStore.getState().setSidebarCollapsed(s.layout.sidebarCollapsed)
            if (s.layout?.inspectorCollapsed !== undefined) useUIStore.getState().setInspectorCollapsed(s.layout.inspectorCollapsed)
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
                // Only persist core flow views, not transient management screens
                const lastViewMode = ['block', 'graph', 'map', 'local-map', 'diff'].includes(state.mainPaneView)
                    ? state.mainPaneView as 'block' | 'graph' | 'map' | 'local-map' | 'diff'
                    : undefined
                
                const lastActiveInspectorTab = ['details', 'ai', 'findings'].includes(state.inspectorTab)
                    ? state.inspectorTab as 'details' | 'ai' | 'findings'
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

    useAppEvents({openDocument})

    const {toggleTheme} = useTheme()

    // Auto-run analysis on flow open when Settings → Rules → "Auto-analyze on
    // flow open" is enabled. Self-contained; reads settings + flow store.
    useAutoAnalyze()

    const showShortcuts = useCallback(() => setShortcutsHelpOpen(true), [])
    useTauriMenuEvents({openDocument, toggleTheme, onShowShortcuts: showShortcuts})

    useEffect(() => {
        const handler = (event: ErrorEvent) => {
            systemApi.logError({message: event.message, stack: event.error?.stack || '', componentStack: '', url: event.filename})
        }
        const rejectionHandler = (event: PromiseRejectionEvent) => {
            systemApi.logError({message: String(event.reason), stack: '', componentStack: '', url: ''})
        }
        window.addEventListener('error', handler)
        window.addEventListener('unhandledrejection', rejectionHandler)
        return () => {
            window.removeEventListener('error', handler)
            window.removeEventListener('unhandledrejection', rejectionHandler)
        }
    }, [])

    const handleSidebarDrag = useCallback((delta: number) => {
        const base = sidebarLiveWidthRef.current ?? layoutRef.current.sidebarWidth
        // Round to whole pixels: clientX deltas are fractional on HiDPI displays,
        // and the persisted width is an int server-side (decode rejects floats).
        const next = Math.round(Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, base + delta)))
        sidebarLiveWidthRef.current = next
        setSidebarLiveWidth(next)
    }, [])

    const handleSidebarResizeEnd = useCallback(() => {
        if (sidebarLiveWidthRef.current !== null) {
            updateLayout({sidebarWidth: sidebarLiveWidthRef.current})
            sidebarLiveWidthRef.current = null
            setSidebarLiveWidth(null)
        }
    }, [updateLayout])

    const handleInspectorDrag = useCallback((delta: number) => {
        const base = inspectorLiveWidthRef.current ?? layoutRef.current.inspectorWidth
        const next = Math.round(Math.min(MAX_INSPECTOR, Math.max(MIN_INSPECTOR, base - delta)))
        inspectorLiveWidthRef.current = next
        setInspectorLiveWidth(next)
    }, [])

    const handleInspectorResizeEnd = useCallback(() => {
        if (inspectorLiveWidthRef.current !== null) {
            updateLayout({inspectorWidth: inspectorLiveWidthRef.current})
            inspectorLiveWidthRef.current = null
            setInspectorLiveWidth(null)
        }
    }, [updateLayout])

    const handleSidebarReset = useCallback(() => updateLayout({sidebarWidth: 280}), [updateLayout])
    const handleInspectorReset = useCallback(() => updateLayout({inspectorWidth: 320}), [updateLayout])

    const handleDragOver = useCallback((e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        if (e.dataTransfer.types.includes('Files')) {
            setDragOver(true)
        }
    }, [])

    const handleDragLeave = useCallback((e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        setDragOver(false)
    }, [])

    const handleDrop = useCallback(async (e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        setDragOver(false)
        const files = e.dataTransfer.files
        if (files.length > 0) {
            const file = files[0]
            if (!file) return
            try {
                if (isTauri()) {
                    const path = (file as File & {path?: string}).path
                    if (path) {
                        const doc = await flowApi.loadFlowFromPath(path)
                        if (doc) openDocument(doc)
                    }
                } else {
                    // Web mode: read content and upload
                    const content = await new Promise<string>((resolve) => {
                        const reader = new FileReader()
                        reader.onload = (e) => resolve(e.target?.result as string)
                        reader.readAsText(file)
                    })
                    const doc = await flowApi.uploadFlow(file.name, {[file.name]: content})
                    if (doc) openDocument(doc)
                }
            } catch (err) {
                console.error('Failed to open dropped file:', err)
                toast.error('Failed to open file', {description: String(err)})
            }
        }
    }, [openDocument])

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
        <div
            ref={dropRef}
                    className={`flex flex-col h-screen w-screen overflow-hidden bg-surface-0 text-text-primary ${dragOver ? 'ring-2 ring-brand-500 ring-inset' : ''}`}
                    onDragOver={handleDragOver}
                    onDragLeave={handleDragLeave}
                    onDrop={handleDrop}
                >
                    <TitleBar />
                    <div className="flex flex-1 overflow-hidden">
                        {!sidebarCollapsed && (
                            <>
                                <div
                                    className="flex-shrink-0 overflow-hidden border-r border-border-subtle"
                                    style={{width: sidebarLiveWidth ?? layout.sidebarWidth}}
                                >
                                    <ErrorBoundary fallback={<div className="p-4 text-text-muted">Sidebar error — try reloading</div>}>
                                        <Sidebar />
                                    </ErrorBoundary>
                                </div>
                                <PaneDivider
                                    onDrag={handleSidebarDrag}
                                    onResizeEnd={handleSidebarResizeEnd}
                                    onDoubleClick={handleSidebarReset}
                                />
                            </>
                        )}
                        <div className="flex-1 overflow-hidden">
                            <ErrorBoundary fallback={<div className="p-4 text-text-muted">Main pane error — try reloading</div>}>
                                <MainPane />
                            </ErrorBoundary>
                        </div>
                        {!inspectorCollapsed && (
                            <>
                                <PaneDivider
                                    onDrag={handleInspectorDrag}
                                    onResizeEnd={handleInspectorResizeEnd}
                                    onDoubleClick={handleInspectorReset}
                                />
                                <div
                                    className="flex-shrink-0 overflow-hidden border-l border-border-subtle"
                                    style={{width: inspectorLiveWidth ?? layout.inspectorWidth}}
                                >
                                    <ErrorBoundary fallback={<div className="p-4 text-text-muted">Inspector error — try reloading</div>}>
                                        <InspectorPanel />
                                    </ErrorBoundary>
                                </div>
                            </>
                        )}
                    </div>
                    <StatusBar />
                    {dragOver && (
                        <div className="fixed inset-0 z-modal bg-surface-overlay flex items-center justify-center pointer-events-none">
                            <div className="text-lg font-medium text-text-primary animate-fade-in">
                                Drop flow file to open
                            </div>
                        </div>
                    )}
                </div>
                <CommandPalette
                    isOpen={commandPaletteOpen}
                    onClose={() => setCommandPaletteOpen(false)}
                    commands={commands}
                />
                <GlobalSearchOverlay
                    isOpen={globalSearchOpen}
                    onClose={() => setGlobalSearchOpen(false)}
                />
                <Suspense fallback={null}>
                    <SettingsModal
                        isOpen={settingsOpen}
                        onClose={() => setSettingsOpen(false)}
                    />
                </Suspense>
                <ShortcutsHelpDialog
                    isOpen={shortcutsHelpOpen}
                    onClose={() => setShortcutsHelpOpen(false)}
                />
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
