import {useKeyboard} from './useKeyboard'
import {useUIStore} from '@/stores/uiStore'
import {useEditorStore} from '@/stores/editorStore'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSearchStore} from '@/stores/searchStore'
import {flowApi, analysisApi, exportApi} from '@/api'
import {getPlatformCapabilities} from '@/platform/guards'
import {exportFindingsCSV, exportFindingsHTML} from '@/lib/findingsExport'
import {logger} from '@/lib/logger'
import type {FlowDocument as DomainFlowDocument, AnalysisReport} from '@/types'
import type {useToast} from '@/components/shared'

export function useAppShortcuts(deps: {
  openDocument: (doc: DomainFlowDocument | null) => void
  toggleTheme: () => void
  toast: ReturnType<typeof useToast>
  setShortcutsHelpOpen: (v: boolean | ((prev: boolean) => boolean)) => void
}) {
  const {openDocument, toggleTheme, toast, setShortcutsHelpOpen} = deps

  useKeyboard({
    scope: 'global',
    handlers: {
      'nav.palette': () => useUIStore.getState().setCommandPaletteOpen(v => !v),
      'nav.search': () => {
        if (useUIStore.getState().sidebarCollapsed) useUIStore.getState().toggleSidebar()
        useSearchStore.getState().requestFocus()
      },
      'nav.search.global': () => useUIStore.getState().setGlobalSearchOpen(v => !v),
      'view.toggle.sidebar': () => useUIStore.getState().toggleSidebar(),
      'view.toggle.inspector': () => useUIStore.getState().toggleInspector(),
      'nav.settings': () => useUIStore.getState().toggleSettings(),
      'help.shortcuts': () => setShortcutsHelpOpen(true),
      'view.split.toggle': () => useEditorStore.getState().splitRight(),
      'view.toggle.mode': () => {
        const current = useUIStore.getState().mainPaneView
        useUIStore.getState().setMainPaneView(current === 'block' ? 'graph' : 'block')
      },
      'view.tab.details': () => {
        useUIStore.getState().setInspectorTab('details')
        if (useUIStore.getState().inspectorCollapsed) useUIStore.getState().toggleInspector()
      },
      'view.tab.ai': () => {
        useUIStore.getState().setInspectorTab('ai')
        if (useUIStore.getState().inspectorCollapsed) useUIStore.getState().toggleInspector()
      },
      'view.tab.findings': () => {
        useUIStore.getState().setInspectorTab('findings')
        if (useUIStore.getState().inspectorCollapsed) useUIStore.getState().toggleInspector()
      },
      'analysis.run': async () => {
        if (!useFlowStore.getState().document) return
        const {setAnalyzing, setReport} = useAnalysisStore.getState()
        useUIStore.getState().setInspectorTab('findings')
        if (useUIStore.getState().inspectorCollapsed) useUIStore.getState().toggleInspector()
        setAnalyzing(true)
        try {
          const r = await analysisApi.analyzeFlow()
          const doc = useFlowStore.getState().document
          if (r && doc) setReport(doc.id, r as AnalysisReport)
        } catch (e) {
          logger.warn('analysis failed:', e)
          toast.error('Analysis failed', {description: String(e)})
        } finally {
          setAnalyzing(false)
        }
      },
      'file.close.tab': () => {
        const {focusedGroupIndex, groups, closeTab} = useEditorStore.getState()
        const group = groups[focusedGroupIndex]
        if (group?.activeTabId) closeTab(focusedGroupIndex, group.activeTabId)
      },
      'file.close.others': () => {
        const {focusedGroupIndex, groups, closeOtherTabs} = useEditorStore.getState()
        const group = groups[focusedGroupIndex]
        if (group?.activeTabId) closeOtherTabs(focusedGroupIndex, group.activeTabId)
      },
      'file.close.all': () => {
        const {focusedGroupIndex, closeAllTabs} = useEditorStore.getState()
        closeAllTabs(focusedGroupIndex)
      },
      'view.group.1': () => useEditorStore.getState().focusGroup(0),
      'view.group.2': () => useEditorStore.getState().focusGroup(1),
      'view.group.3': () => useEditorStore.getState().focusGroup(2),
      'view.group.4': () => useEditorStore.getState().focusGroup(3),
      'view.move.group.right': () => {
        const s = useEditorStore.getState()
        const {focusedGroupIndex, groups} = s
        const activeTabId = groups[focusedGroupIndex]?.activeTabId
        if (activeTabId && focusedGroupIndex < groups.length - 1) {
          s.moveTabToGroup(focusedGroupIndex, activeTabId, focusedGroupIndex + 1)
        }
      },
      'view.move.group.left': () => {
        const s = useEditorStore.getState()
        const {focusedGroupIndex, groups} = s
        const activeTabId = groups[focusedGroupIndex]?.activeTabId
        if (activeTabId && focusedGroupIndex > 0) {
          s.moveTabToGroup(focusedGroupIndex, activeTabId, focusedGroupIndex - 1)
        }
      },
      'nav.search.quick': () => {
        if (useUIStore.getState().sidebarCollapsed) useUIStore.getState().toggleSidebar()
        useSearchStore.getState().requestFocus()
      },
      'view.map': () => useUIStore.getState().setMainPaneView('map'),
      'view.local-map': () => useUIStore.getState().setMainPaneView('local-map'),
      'nav.up.subflow': () => useFlowStore.getState().drillUp(),
      'view.fullscreen': async () => {
        if (!getPlatformCapabilities().nativeWindow) return
        const {getCurrentWindow} = await import('@tauri-apps/api/window')
        const win = getCurrentWindow()
        const isFs = await win.isFullscreen()
        if (isFs) {
          void win.setFullscreen(false)
        } else {
          void win.setFullscreen(true)
        }
      },
      'view.theme.toggle': () => toggleTheme(),
      'file.open': async () => {
        try {
          const doc = await flowApi.openFlowFile()
          if (doc) openDocument(doc as DomainFlowDocument)
        } catch (e) {
          logger.warn('Failed to open file:', e)
          toast.error('Failed to open file', {description: String(e)})
        }
      },
      'file.open.folder': async () => {
        try {
          const doc = await flowApi.openFlowFolder()
          if (doc) openDocument(doc as DomainFlowDocument)
        } catch (e) {
          logger.warn('Failed to open folder:', e)
          toast.error('Failed to open folder', {description: String(e)})
        }
      },
      'file.export.pdf': async () => {
        try {
          await exportApi.exportPDF()
        } catch (e) {
          logger.warn('Export PDF failed:', e)
        }
      },
      'file.export.md': async () => {
        try {
          await exportApi.exportMarkdown()
        } catch (e) {
          logger.warn('Export Markdown failed:', e)
        }
      },
      'analysis.filter.errors': () => useAnalysisStore.getState().setSeverityFilter(new Set(['error'])),
      'analysis.filter.warnings': () => useAnalysisStore.getState().setSeverityFilter(new Set(['warning'])),
      'analysis.filter.info': () => useAnalysisStore.getState().setSeverityFilter(new Set(['info'])),
      'analysis.filter.all': () => useAnalysisStore.getState().setSeverityFilter(new Set(['error', 'warning', 'info'])),
      'analysis.export.csv': () => {
        const d = useFlowStore.getState().document
        const r = d ? useAnalysisStore.getState().reports.get(d.id) : undefined
        if (d && r) exportFindingsCSV(r.findings, d.id)
      },
      'analysis.export.html': async () => {
        const d = useFlowStore.getState().document
        if (!d) return
        try {
          await exportFindingsHTML(d.id)
        } catch (e) {
          logger.warn('HTML export failed:', e)
        }
      },
      'window.reload': () => {
        window.location.reload()
      },
      'window.quit': () => {
        if (!getPlatformCapabilities().nativeWindow) return
        void import('@tauri-apps/api/window').then(({getCurrentWindow}) => getCurrentWindow().close())
      },
    },
  })
}
