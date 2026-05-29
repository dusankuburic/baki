import {useCallback, useEffect, useRef, useState, useMemo, lazy, Suspense} from 'react'
import {ErrorBoundary, ToastProvider} from './components/shared'
import CommandPalette from './components/search/CommandPalette'
import GlobalSearchOverlay from './components/search/GlobalSearchOverlay'
import ShortcutsHelpDialog from './components/search/ShortcutsHelpDialog'

const SettingsModal = lazy(() => import('./components/settings/SettingsModal'))
import {useSettingsStore, onSettingsLoaded} from './stores/settingsStore'
import {useUIStore} from './stores/uiStore'
import {useAuthStore} from './stores/authStore'
import {useFlowStore} from './stores/flowStore'
import {useChatStore} from './stores/chatStore'
import {useSearchStore} from './stores/searchStore'
import {useAnalysisStore} from './stores/analysisStore'
import {usePresenceStore} from './stores/presenceStore'
import {useKeyboard} from './hooks/useKeyboard'
import {useTheme} from './hooks/useTheme'
import {useTauriMenuEvents} from './hooks/useTauriMenuEvents'
import TitleBar from './components/layout/TitleBar'
import Sidebar from './components/layout/Sidebar'
import MainPane from './components/layout/MainPane'
import InspectorPanel from './components/layout/InspectorPanel'
import StatusBar from './components/layout/StatusBar'
import PaneDivider from './components/layout/PaneDivider'
import {flowApi, analysisApi, exportApi, systemApi} from '@/api'
import {subscribeToEvents} from '@/api/client'
import {isTauri} from '@/platform/guards'
import type {FlowDocument as DomainFlowDocument, RecentFile} from './types/domain'

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 480
const MIN_INSPECTOR = 280
const MAX_INSPECTOR = 560

export default function App() {
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
    const requestSearchFocus = useSearchStore(s => s.requestFocus)
    const [dragOver, setDragOver] = useState(false)
    const [shortcutsHelpOpen, setShortcutsHelpOpen] = useState(false)
    const dropRef = useRef<HTMLDivElement>(null)
    const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])

    useEffect(() => {
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (files) setRecentFiles(files) })
            .catch(() => {})
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
            if (s.ai?.activeProvider) useChatStore.getState().setProvider(s.ai.activeProvider as any)
        })
        loadFromBackend()
        return unsub
    }, [loadFromBackend])

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
                    ? state.mainPaneView as any
                    : undefined
                
                const lastActiveInspectorTab = ['details', 'ai', 'findings'].includes(state.inspectorTab)
                    ? state.inspectorTab as any
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

    useEffect(() => {
        // Use a cancelled flag + sync unsub ref so cleanup is synchronous (React
        // does not await async cleanup functions). openDocument is in deps so the
        // handler always calls the current version after login/store resets.
        let unsub: (() => void) | null = null
        let cancelled = false
        subscribeToEvents((ev) => {
            if (ev.name === 'flow:parse-progress') {
                useFlowStore.setState({parseProgress: ev.data.percent ?? 0, isParsing: true})
            } else if (ev.name === 'flow:loaded') {
                if (ev.data) openDocument(ev.data as any)
            } else if (ev.name === 'flow:load-error') {
                useFlowStore.getState().setParseError(ev.data?.error ?? 'Unknown error')
            }
        }).then(fn => { if (!cancelled) unsub = fn; else fn() })
        return () => {
            cancelled = true
            unsub?.()
        }
    }, [openDocument])

    const {toggleTheme} = useTheme()

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
        const next = Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, layout.sidebarWidth + delta))
        updateLayout({sidebarWidth: next})
    }, [layout.sidebarWidth, updateLayout])

    const handleInspectorDrag = useCallback((delta: number) => {
        const next = Math.min(MAX_INSPECTOR, Math.max(MIN_INSPECTOR, layout.inspectorWidth - delta))
        updateLayout({inspectorWidth: next})
    }, [layout.inspectorWidth, updateLayout])

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
                    const path = (file as any).path as string
                    if (path) {
                        const doc = await flowApi.loadFlowFromPath(path)
                        if (doc) openDocument(doc as any)
                    }
                } else {
                    // Web mode: read content and upload
                    const content = await new Promise<string>((resolve) => {
                        const reader = new FileReader()
                        reader.onload = (e) => resolve(e.target?.result as string)
                        reader.readAsText(file)
                    })
                    const doc = await flowApi.uploadFlow(file.name, {[file.name]: content})
                    if (doc) openDocument(doc as any)
                }
            } catch (err) {
                console.error('Failed to open dropped file:', err)
            }
        }
    }, [openDocument])

    useKeyboard({
        scope: 'global',
        handlers: {
            'nav.palette': () => setCommandPaletteOpen(v => !v),
            'nav.search': () => {
                // mod+f: focus the sidebar search bar (mirrors VS Code behaviour)
                if (sidebarCollapsed) toggleSidebar()
                requestSearchFocus()
            },
            'nav.search.global': () => setGlobalSearchOpen(v => !v),
            'view.toggle.sidebar': () => toggleSidebar(),
            'view.toggle.inspector': () => toggleInspector(),
            'nav.settings': () => toggleSettings(),
            'help.shortcuts': () => setShortcutsHelpOpen(true),
            'view.split.toggle': () => useFlowStore.getState().splitRight(),
            'view.toggle.mode': () => {
                const current = useUIStore.getState().mainPaneView
                setMainPaneView(current === 'block' ? 'graph' : 'block')
            },
            'view.tab.details': () => {
                useUIStore.getState().setInspectorTab('details')
                if (useUIStore.getState().inspectorCollapsed) toggleInspector()
            },
            'view.tab.ai': () => {
                useUIStore.getState().setInspectorTab('ai')
                if (useUIStore.getState().inspectorCollapsed) toggleInspector()
            },
            'view.tab.findings': () => {
                useUIStore.getState().setInspectorTab('findings')
                if (useUIStore.getState().inspectorCollapsed) toggleInspector()
            },
            'analysis.run': async () => {
                if (!useFlowStore.getState().document) return
                const {setAnalyzing, setReport} = useAnalysisStore.getState()
                useUIStore.getState().setInspectorTab('findings')
                if (useUIStore.getState().inspectorCollapsed) toggleInspector()
                setAnalyzing(true)
                try {
                    const r = await analysisApi.analyzeFlow()
                    const doc = useFlowStore.getState().document
                    if (r && doc) setReport(doc.id, r as any)
                } catch (e) {
                    console.error('analysis failed:', e)
                } finally {
                    setAnalyzing(false)
                }
            },
            'file.close.tab': () => {
                const {focusedGroupIndex, groups, closeTab} = useFlowStore.getState()
                const group = groups[focusedGroupIndex]
                if (group?.activeTabId) closeTab(focusedGroupIndex, group.activeTabId)
            },
            'file.close.others': () => {
                const {focusedGroupIndex, groups, closeOtherTabs} = useFlowStore.getState()
                const group = groups[focusedGroupIndex]
                if (group?.activeTabId) closeOtherTabs(focusedGroupIndex, group.activeTabId)
            },
            'file.close.all': () => {
                const {focusedGroupIndex, closeAllTabs} = useFlowStore.getState()
                closeAllTabs(focusedGroupIndex)
            },
            'view.group.1': () => useFlowStore.getState().focusGroup(0),
            'view.group.2': () => useFlowStore.getState().focusGroup(1),
            'view.group.3': () => useFlowStore.getState().focusGroup(2),
            'view.group.4': () => useFlowStore.getState().focusGroup(3),
            'view.move.group.right': () => {
                const s = useFlowStore.getState()
                const {focusedGroupIndex, groups} = s
                const activeTabId = groups[focusedGroupIndex]?.activeTabId
                if (activeTabId && focusedGroupIndex < groups.length - 1) {
                    s.moveTabToGroup(focusedGroupIndex, activeTabId, focusedGroupIndex + 1)
                }
            },
            'view.move.group.left': () => {
                const s = useFlowStore.getState()
                const {focusedGroupIndex, groups} = s
                const activeTabId = groups[focusedGroupIndex]?.activeTabId
                if (activeTabId && focusedGroupIndex > 0) {
                    s.moveTabToGroup(focusedGroupIndex, activeTabId, focusedGroupIndex - 1)
                }
            },
            'nav.search.quick': () => {
                if (sidebarCollapsed) toggleSidebar()
                requestSearchFocus()
            },
            'view.map': () => setMainPaneView('map'),
            'view.local-map': () => setMainPaneView('local-map'),
            'nav.up.subflow': () => useFlowStore.getState().drillUp(),
            'view.fullscreen': async () => {
                if (!isTauri()) return
                const { getCurrentWindow } = await import('@tauri-apps/api/window')
                const win = getCurrentWindow()
                const isFs = await win.isFullscreen()
                if (isFs) { win.setFullscreen(false) } else { win.setFullscreen(true) }
            },
            'view.theme.toggle': () => toggleTheme(),
            'file.open': async () => {
                const doc = await flowApi.openFlowFile()
                if (doc) openDocument(doc as any)
            },
            'file.open.folder': async () => {
                const doc = await flowApi.openFlowFolder()
                if (doc) openDocument(doc as any)
            },
            'file.export.pdf': async () => { try { await exportApi.exportPDF() } catch { /* ignore */ } },
            'file.export.md': async () => { try { await exportApi.exportMarkdown() } catch { /* ignore */ } },
            'analysis.filter.errors': () => useAnalysisStore.getState().setSeverityFilter(new Set(['error'])),
            'analysis.filter.warnings': () => useAnalysisStore.getState().setSeverityFilter(new Set(['warning'])),
            'analysis.filter.info': () => useAnalysisStore.getState().setSeverityFilter(new Set(['info'])),
            'analysis.filter.all': () => useAnalysisStore.getState().setSeverityFilter(new Set(['error', 'warning', 'info'])),
            'window.reload': () => { window.location.reload() },
            'window.quit': () => {
                if (!isTauri()) return
                import('@tauri-apps/api/window').then(({ getCurrentWindow }) => getCurrentWindow().close())
            },
        },
    })

    const commands = useMemo(() => {
        const cmds: {id: string; label: string; section: string; shortcut?: string[]; onSelect: () => void}[] = [
            {id: 'file.open', label: 'Open Flow File', section: 'File', shortcut: ['mod', 'o'], onSelect: async () => {
                const doc = await flowApi.openFlowFile()
                if (doc) openDocument(doc as DomainFlowDocument)
            }},
            {id: 'file.open.folder', label: 'Open Folder', section: 'File', shortcut: ['mod', 'shift', 'o'], onSelect: async () => {
                const doc = await flowApi.openFlowFolder()
                if (doc) openDocument(doc as DomainFlowDocument)
            }},
            {id: 'view.toggle-sidebar', label: 'Toggle Sidebar', section: 'View', shortcut: ['mod', 'b'], onSelect: toggleSidebar},
            {id: 'view.toggle-inspector', label: 'Toggle Inspector', section: 'View', shortcut: ['mod', 'i'], onSelect: toggleInspector},
            {id: 'view.block', label: 'Block View', section: 'View', onSelect: () => setMainPaneView('block')},
            {id: 'view.graph', label: 'Graph View', section: 'View', onSelect: () => setMainPaneView('graph')},
            {id: 'view.local-map', label: 'Local Subflow Map', section: 'View', shortcut: ['mod', 'shift', 'm'], onSelect: () => setMainPaneView('local-map')},
            {id: 'view.map', label: 'Subflow Map', section: 'View', shortcut: ['mod', 'm'], onSelect: () => setMainPaneView('map')},
            {id: 'view.split', label: 'Split Editor Right', section: 'View', shortcut: ['mod', '\\'], onSelect: () => useFlowStore.getState().splitRight()},
            {id: 'settings', label: 'Open Settings', section: 'Navigation', shortcut: ['mod', ','], onSelect: toggleSettings},
            {id: 'search.focus', label: 'Focus Search', section: 'Search', shortcut: ['/'], onSelect: () => {
                if (sidebarCollapsed) toggleSidebar()
                requestSearchFocus()
            }},
            {id: 'analysis.run', label: 'Run Analysis', section: 'Analysis', shortcut: ['mod', 'shift', 'r'], onSelect: async () => {
                if (!document) return
                const setAnalyzing = useAnalysisStore.getState().setAnalyzing
                const setReport = useAnalysisStore.getState().setReport
                const setInspectorTab = useUIStore.getState().setInspectorTab
                setInspectorTab('findings')
                setAnalyzing(true)
                try {
                    const r = await analysisApi.analyzeFlow()
                    if (r) setReport(document.id, r as any)
                } catch (e) {
                    console.error('analysis failed:', e)
                } finally {
                    setAnalyzing(false)
                }
            }},
            {id: 'file.export.pdf', label: 'Export PDF', section: 'File', shortcut: ['mod', 'e'], onSelect: async () => {
                try { await exportApi.exportPDF() } catch (e) { console.error('Export PDF failed:', e) }
            }},
            {id: 'file.export.md', label: 'Export Markdown', section: 'File', shortcut: ['mod', 'shift', 'e'], onSelect: async () => {
                try { await exportApi.exportMarkdown() } catch (e) { console.error('Export Markdown failed:', e) }
            }},
            {id: 'nav.profile', label: 'User Profile', section: 'Navigation', onSelect: () => setMainPaneView('profile')},
        ]

        if (user?.role === 'admin') {
            cmds.push({id: 'nav.admin', label: 'Admin Dashboard', section: 'Navigation', onSelect: () => setMainPaneView('admin')})
        }

        if (isTauri()) {
            for (const f of recentFiles.slice(0, 5)) {
                cmds.push({
                    id: `recent:${f.path}`,
                    label: f.name,
                    section: 'Recent Files',
                    onSelect: async () => {
                        const doc = await flowApi.loadFlowFromPath(f.path)
                        if (doc) openDocument(doc as DomainFlowDocument)
                    },
                })
            }
        }

        if (document) {
            const blockCmds = extractBlockCommands(document)
            for (const bc of blockCmds.slice(0, 20)) {
                cmds.push(bc)
            }
        }

        return cmds
    }, [toggleSidebar, toggleInspector, toggleSettings, setMainPaneView, setDocument, recentFiles, requestSearchFocus, sidebarCollapsed, document, user?.role])

function extractBlockCommands(doc: DomainFlowDocument) {
    const cmds: {id: string; label: string; section: string; onSelect: () => void}[] = []
    const collectBlocks = (blocks: any[], subflowId: string) => {
        for (const block of blocks) {
            cmds.push({
                id: `block:${block.id}`,
                label: block.name,
                section: 'Blocks',
                onSelect: () => {
                    useFlowStore.getState().selectBlock(block.id)
                    useFlowStore.getState().selectSubflow(subflowId)
                },
            })
            if (block.children?.length) {
                collectBlocks(block.children, subflowId)
            }
        }
    }
    for (const sf of doc.subflows) {
        collectBlocks(sf.blocks, sf.id)
    }
    return cmds
}

    return (
        <ErrorBoundary>
            <ToastProvider>
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
                                    style={{width: layout.sidebarWidth}}
                                >
                                    <Sidebar />
                                </div>
                                <PaneDivider
                                    onDrag={handleSidebarDrag}
                                    onResizeEnd={() => {}}
                                    onDoubleClick={handleSidebarReset}
                                />
                            </>
                        )}
                        <div className="flex-1 overflow-hidden">
                            <MainPane />
                        </div>
                        {!inspectorCollapsed && (
                            <>
                                <PaneDivider
                                    onDrag={handleInspectorDrag}
                                    onResizeEnd={() => {}}
                                    onDoubleClick={handleInspectorReset}
                                />
                                <div
                                    className="flex-shrink-0 overflow-hidden border-l border-border-subtle"
                                    style={{width: layout.inspectorWidth}}
                                >
                                    <InspectorPanel />
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
            </ToastProvider>
        </ErrorBoundary>
    )
}
