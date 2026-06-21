import {useMemo} from 'react'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {useEditorStore} from '@/stores/editorStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {flowApi, analysisApi, exportApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {exportFindingsCSV, exportFindingsHTML} from '@/lib/findingsExport'
import {logger} from '@/lib/logger'
import {THEME_REGISTRY} from '@/lib/themeRegistry'
import type {FlowDocument as DomainFlowDocument, RecentFile, AnalysisReport, Block} from '@/types'
import type {useToast} from '@/components/shared'

function extractBlockCommands(doc: DomainFlowDocument) {
    const cmds: {id: string; label: string; section: string; onSelect: () => void}[] = []
    const collectBlocks = (blocks: Block[], subflowId: string) => {
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

export type CommandItem = {id: string; label: string; section: string; shortcut?: string[]; onSelect: () => void}

export function useCommandList(deps: {
    openDocument: (doc: DomainFlowDocument | null) => void
    toggleSidebar: () => void
    toggleInspector: () => void
    toggleSettings: () => void
    setMainPaneView: (v: 'map' | 'block' | 'graph' | 'local-map' | 'diff' | 'profile' | 'admin' | 'dashboard' | 'portfolio') => void
    requestSearchFocus: () => void
    recentFiles: RecentFile[]
    sidebarCollapsed: boolean
    document: DomainFlowDocument | null
    user: { role?: string } | null
    toast: ReturnType<typeof useToast>
}): CommandItem[] {
    const {
        openDocument, toggleSidebar, toggleInspector, toggleSettings,
        setMainPaneView, requestSearchFocus, recentFiles, sidebarCollapsed,
        document, user, toast,
    } = deps

    return useMemo(() => {
        const cmds: CommandItem[] = [
            {id: 'file.open', label: 'Open Flow File', section: 'File', shortcut: ['mod', 'o'], onSelect: async () => {
                try {
                    const doc = await flowApi.openFlowFile()
                    if (doc) openDocument(doc as DomainFlowDocument)
                } catch (e) {
                    logger.warn('Failed to open file:', e)
                    toast.error('Failed to open file', {description: String(e)})
                }
            }},
            {id: 'file.open.folder', label: 'Open Folder', section: 'File', shortcut: ['mod', 'shift', 'o'], onSelect: async () => {
                try {
                    const doc = await flowApi.openFlowFolder()
                    if (doc) openDocument(doc as DomainFlowDocument)
                } catch (e) {
                    logger.warn('Failed to open folder:', e)
                    toast.error('Failed to open folder', {description: String(e)})
                }
            }},
            {id: 'view.toggle-sidebar', label: 'Toggle Sidebar', section: 'View', shortcut: ['mod', 'b'], onSelect: toggleSidebar},
            {id: 'view.toggle-inspector', label: 'Toggle Inspector', section: 'View', shortcut: ['mod', 'i'], onSelect: toggleInspector},
            {id: 'view.block', label: 'Block View', section: 'View', onSelect: () => setMainPaneView('block')},
            {id: 'view.graph', label: 'Graph View', section: 'View', onSelect: () => setMainPaneView('graph')},
            {id: 'view.local-map', label: 'Local Subflow Map', section: 'View', shortcut: ['mod', 'shift', 'm'], onSelect: () => setMainPaneView('local-map')},
            {id: 'view.map', label: 'Subflow Map', section: 'View', shortcut: ['mod', 'm'], onSelect: () => setMainPaneView('map')},
            {id: 'view.split', label: 'Split Editor Right', section: 'View', shortcut: ['mod', '\\'], onSelect: () => useEditorStore.getState().splitRight()},
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
                    if (r) setReport(document.id, r as AnalysisReport)
                } catch (e) {
                    logger.warn('analysis failed:', e)
                    toast.error('Analysis failed', {description: String(e)})
                } finally {
                    setAnalyzing(false)
                }
            }},
            {id: 'analysis.export.csv', label: 'Export Findings as CSV', section: 'Analysis', shortcut: ['mod', 'alt', 'e'], onSelect: () => {
                const r = document ? useAnalysisStore.getState().reports.get(document.id) : undefined
                if (document && r) exportFindingsCSV(r, document.id)
            }},
            {id: 'analysis.export.html', label: 'Export Findings as HTML', section: 'Analysis', shortcut: ['mod', 'shift', 'h'], onSelect: async () => {
                if (!document) return
                try { await exportFindingsHTML(document.id) } catch (e) { logger.warn('HTML export failed:', e) }
            }},
            {id: 'analysis.filter.all', label: 'Findings: Show All Severities', section: 'Analysis', shortcut: ['mod', 'shift', '0'], onSelect: () => {
                useAnalysisStore.getState().setSeverityFilter(new Set(['error', 'warning', 'info']))
            }},
            {id: 'file.export.pdf', label: 'Export PDF', section: 'File', shortcut: ['mod', 'e'], onSelect: async () => {
                try { await exportApi.exportPDF() } catch (e) { logger.warn('Export PDF failed:', e) }
            }},
            {id: 'file.export.md', label: 'Export Markdown', section: 'File', shortcut: ['mod', 'shift', 'e'], onSelect: async () => {
                try { await exportApi.exportMarkdown() } catch (e) { logger.warn('Export Markdown failed:', e) }
            }},
            {id: 'view.dashboard', label: 'Analysis Dashboard', section: 'Analysis', onSelect: () => setMainPaneView('dashboard')},
            {id: 'nav.profile', label: 'User Profile', section: 'Navigation', onSelect: () => setMainPaneView('profile')},

            // ---- Appearance: theme switching via command palette ----
            {id: 'theme.toggle', label: 'Toggle Light/Dark Theme', section: 'Appearance', shortcut: ['mod', 'shift', 't'], onSelect: () => {
                const resolved = useUIStore.getState().resolvedTheme
                useSettingsStore.getState().updateAppearance({theme: resolved === 'light' ? 'dark' : 'light'})
            }},
            {id: 'theme.system', label: 'Theme: System (Follow OS)', section: 'Appearance', onSelect: () => useSettingsStore.getState().updateAppearance({theme: 'system'})},
            ...THEME_REGISTRY.map(t => ({
                id: `theme.${t.id}`,
                label: `Theme: ${t.label}`,
                section: 'Appearance',
                onSelect: () => useSettingsStore.getState().updateAppearance({theme: t.id}),
            })),
        ]

        if (user?.role === 'admin') {
            cmds.push({id: 'nav.admin', label: 'Admin Dashboard', section: 'Navigation', onSelect: () => setMainPaneView('admin')})
        }

        // The portfolio is an org-wide governance view — cloud mode only.
        if (!isTauri()) {
            cmds.push({id: 'view.portfolio', label: 'Flow Portfolio', section: 'Analysis', onSelect: () => setMainPaneView('portfolio')})
        }

        if (isTauri()) {
            for (const f of recentFiles.slice(0, 5)) {
                cmds.push({
                    id: `recent:${f.path}`,
                    label: f.name,
                    section: 'Recent Files',
                    onSelect: async () => {
                        try {
                            const doc = await flowApi.loadFlowFromPath(f.path)
                            if (doc) openDocument(doc as DomainFlowDocument)
                        } catch (e) {
                            logger.warn('Failed to load recent file:', e)
                            toast.error('Failed to load file', {description: String(e)})
                        }
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
    }, [toggleSidebar, toggleInspector, toggleSettings, setMainPaneView, openDocument, recentFiles, requestSearchFocus, sidebarCollapsed, document, user?.role, toast])
}
