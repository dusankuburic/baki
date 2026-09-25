import {
  List,
  Network,
  Map,
  History,
  Minus,
  Plus,
  Maximize2,
  Download,
  Expand,
  Shrink,
  ChevronLeft,
  ChevronRight,
  MapPin,
  Flame,
  LayoutDashboard,
  BarChart3,
  User,
  Shield,
  Cloud,
  RefreshCw,
  Code2,
  FileCode,
  MoreHorizontal,
} from 'lucide-react'
import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import SegmentedControl from '@/components/shared/SegmentedControl'
import IconButton from '@/components/shared/IconButton'
import {Dropdown, useConfirm, type DropdownItem} from '@/components/shared'
import {useUIStore, isSystemView} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSystemStore} from '@/stores/systemStore'
import {useAuthStore} from '@/stores/authStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFullscreen} from '@/hooks/useFullscreen'
import {exportApi, flowApi, analysisApi} from '@/api'
import {getPlatformCapabilities} from '@/platform/guards'
import {useToast} from '@/components/shared/Toast'
import type {FlowDiff, AnalysisReport} from '@/types'

type SystemView = 'home' | 'dashboard' | 'library' | 'profile' | 'admin'

type FlowView = 'block' | 'graph' | 'local-map' | 'map' | 'diff'
// The raw-text editor is one more way of looking at the flow, so it sits in
// the view switcher with the others. It used to be a stray button absolutely
// positioned over the toolbar, where it overlapped the export icons.
type ToolbarView = FlowView | 'source'

function ToolbarDivider() {
  return <div className="main-toolbar-sep w-px h-5 bg-border-subtle mx-1 shrink-0" aria-hidden="true" />
}

// One toggle for both toolbar variants. Icon and label follow the document's
// fullscreen state, so the same button leaves fullscreen again; it used to
// request fullscreen on every click, and the second click did nothing.
function FullscreenButton() {
  const {t} = useTranslation('shell')
  const {isFullscreen, toggleFullscreen} = useFullscreen()
  return (
    <IconButton
      icon={isFullscreen ? Shrink : Expand}
      size="sm"
      label={isFullscreen ? t('toolbar.exitFullscreen') : t('toolbar.fullscreen')}
      onClick={toggleFullscreen}
    />
  )
}

// The toolbar answers to its own width (.main-toolbar in index.css): the view
// switcher drops its labels, then the secondary actions fold into the "More"
// menu, then the history buttons go. Nothing is pushed outside the pane.
export default function MainPaneToolbar() {
  const mainPaneView = useUIStore(s => s.mainPaneView)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const complexityMode = useUIStore(s => s.complexityMode)
  const toggleComplexityMode = useUIStore(s => s.toggleComplexityMode)
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const setActiveDiff = useUIStore(s => s.setActiveDiff)
  const showSource = useUIStore(s => s.showSourceEditor)
  const setShowSource = useUIStore(s => s.setShowSourceEditor)
  const sourceDirty = useUIStore(s => s.sourceEditorDirty)
  const document = useFlowStore(s => s.document)
  const navigationHistory = useFlowStore(s => s.navigationHistory)
  const historyIndex = useFlowStore(s => s.historyIndex)
  const goBack = useFlowStore(s => s.goBack)
  const goForward = useFlowStore(s => s.goForward)
  const setDocument = useFlowStore(s => s.setDocument)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const setReport = useAnalysisStore(s => s.setReport)
  const [reimporting, setReimporting] = useState(false)
  const {t} = useTranslation('shell')
  const {confirm} = useConfirm()

  const toast = useToast()
  const userRole = useAuthStore(s => s.user?.role)
  const sessionAnalytics = useSystemStore(s => s.info?.capabilities.sessionAnalytics ?? false)

  if (isSystemView(mainPaneView)) {
    const view = mainPaneView as SystemView
    const title =
      view === 'dashboard'
        ? sessionAnalytics
          ? t('titles.sessionDashboard')
          : t('titles.dashboard')
        : t(`titles.${view}`)
    const systemOptions: {value: SystemView; label: string; icon: typeof LayoutDashboard}[] = [
      {value: 'home', label: t('nav.home'), icon: LayoutDashboard},
      {value: 'dashboard', label: t('nav.analytics'), icon: BarChart3},
      {value: 'library', label: t('nav.library'), icon: Cloud},
      {value: 'profile', label: t('nav.profile'), icon: User},
    ]
    if (userRole === 'admin') systemOptions.push({value: 'admin', label: t('nav.admin'), icon: Shield})
    return (
      <div className="main-toolbar flex items-center h-12 px-3 gap-3 min-w-0 border-b border-border-default bg-surface-1">
        <SegmentedControl
          value={mainPaneView as SystemView}
          onChange={v => setMainPaneView(v)}
          options={systemOptions}
          size="sm"
          label={t('toolbar.sections')}
          collapseLabels
        />
        <div className="flex-1 min-w-0 flex items-center">
          <span className="text-sm font-medium text-text-primary truncate">{title}</span>
        </div>
        <FullscreenButton />
      </div>
    )
  }

  const handleExport = async (format: 'pdf' | 'markdown' | 'html') => {
    try {
      const fn =
        format === 'pdf' ? exportApi.exportPDF : format === 'html' ? exportApi.exportHTML : exportApi.exportMarkdown
      const path = await fn()
      if (path) {
        toast.success(t('toasts.exportedTo', {path}))
      }
    } catch (e) {
      toast.error(t('toasts.exportFailed', {message: (e as Error).message}))
    }
  }

  const handleCompare = async () => {
    try {
      const path = await exportApi.pickFile('Select Old Version for Comparison')
      if (!path) return

      toast.info(t('toasts.comparing'))
      const diff = await exportApi.compareCurrentWith(path)
      if (diff) {
        setActiveDiff(diff as FlowDiff)
        setMainPaneView('diff')
        toast.success(t('toasts.comparisonComplete'))
      }
    } catch (e) {
      toast.error(t('toasts.comparisonFailed', {message: (e as Error).message}))
    }
  }

  // handleReimport re-reads the current flow's source file (desktop), re-
  // parses it, and auto-re-analyzes — the pragmatic fix-loop accelerator for
  // after the user edits in Power Automate Desktop. One click instead of
  // navigating the file picker + clicking Analyze.
  const handleReimport = async () => {
    if (!document) return
    setReimporting(true)
    const gen = beginAnalyzing()
    try {
      const fresh = await flowApi.reimport(document.id)
      setDocument(fresh)
      const r = await analysisApi.analyzeFlow()
      if (r) setReport(fresh.id, r as AnalysisReport)
      toast.success(t('toasts.reimported'))
    } catch (e) {
      toast.error(t('toasts.reimportFailed', {message: (e as Error).message}))
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
      setReimporting(false)
    }
  }

  // Leaving the source editor from the view switcher goes through the same
  // guard as the editor's own close button: unsaved edits are confirmed
  // away, never dropped by a view click.
  const handleViewChange = async (view: ToolbarView) => {
    if (view === 'source') {
      setShowSource(true)
      return
    }
    if (showSource) {
      if (sourceDirty) {
        const ok = await confirm({
          title: t('mainPane.discardSourceTitle'),
          message: t('mainPane.discardSourceMessage'),
          danger: true,
          confirmLabel: t('mainPane.discardSourceConfirm'),
        })
        if (!ok) return
      }
      setShowSource(false)
    }
    setMainPaneView(view)
  }

  const isGraphView = mainPaneView === 'graph' || mainPaneView === 'map' || mainPaneView === 'local-map'
  const toolbarView: ToolbarView = showSource ? 'source' : (mainPaneView as FlowView)
  const viewOptions: {value: ToolbarView; label: string; icon: typeof List}[] = [
    {value: 'block', label: 'Block', icon: List},
    {value: 'graph', label: 'Graph', icon: Network},
    {value: 'local-map', label: 'Local Map', icon: MapPin},
    {value: 'map', label: 'Map', icon: Map},
    {value: 'diff', label: 'Diff', icon: History},
  ]
  if (document) viewOptions.push({value: 'source', label: t('toolbar.source'), icon: Code2})

  // Re-import re-reads the flow's source FILE: it needs a filesystem and a
  // file-backed flow. It was always rendered and answered 403 in cloud mode
  // / 400 for uploaded flows.
  const canReimport = getPlatformCapabilities().fileSystem && !!document?.filePath
  const reimportLabel = reimporting ? t('toolbar.reimporting') : t('toolbar.reimport')

  // The same actions as the inline buttons, for the "More" menu that replaces
  // them in a narrow pane (see .main-toolbar-narrow). The graph views' Fit
  // and PNG export fold in too; zoom in/out stay inline.
  const secondaryItems: DropdownItem[] = [
    ...(isGraphView && !showSource
      ? [
          {
            type: 'item' as const,
            label: t('toolbar.fitToScreen'),
            icon: Maximize2,
            onSelect: () => window.dispatchEvent(new Event('graph:fit')),
          },
          {
            type: 'item' as const,
            label: t('toolbar.exportGraphPng'),
            icon: Download,
            onSelect: () => window.dispatchEvent(new Event('graph:export-png')),
          },
          {type: 'separator' as const},
        ]
      : []),
    ...(canReimport
      ? [
          {
            type: 'item' as const,
            label: reimportLabel,
            icon: RefreshCw,
            disabled: reimporting,
            onSelect: () => void handleReimport(),
          },
        ]
      : []),
    {type: 'item', label: t('toolbar.exportPdf'), icon: Download, onSelect: () => void handleExport('pdf')},
    {type: 'item', label: t('toolbar.exportHtml'), icon: FileCode, onSelect: () => void handleExport('html')},
  ]

  return (
    <div
      className="main-toolbar flex items-center h-12 px-3 gap-2 min-w-0 border-b border-border-default bg-surface-1"
      data-tools={isGraphView ? 'graph' : undefined}
    >
      <div className="main-toolbar-nav flex items-center gap-0.5 shrink-0">
        <IconButton
          icon={ChevronLeft}
          size="sm"
          label={t('toolbar.goBack')}
          disabled={historyIndex <= 0}
          onClick={goBack}
        />
        <IconButton
          icon={ChevronRight}
          size="sm"
          label={t('toolbar.goForward')}
          disabled={historyIndex >= navigationHistory.length - 1}
          onClick={goForward}
        />
        <ToolbarDivider />
      </div>

      <SegmentedControl
        value={toolbarView}
        onChange={v => void handleViewChange(v)}
        options={viewOptions}
        size="sm"
        label={t('toolbar.views')}
        collapseLabels
      />

      {/* The center breadcrumb copy is GONE (U5a.3): the interactive
          Breadcrumbs bar renders directly under the tab strip — showing the
          same path twice in one pane was chrome, not information. The diff
          label stays (it replaces the editor view, not a duplicate). */}
      <div className="flex-1 min-w-0 flex items-center justify-center">
        {mainPaneView === 'diff' && !showSource && (
          <div className="flex items-center gap-2 min-w-0 text-sm font-medium text-brand-500">
            <History size={16} className="shrink-0" />
            <span className="truncate">Version Comparison</span>
          </div>
        )}
      </div>

      <div className="flex items-center gap-1 shrink-0">
        {/* View-specific tools; none apply to the source editor. */}
        {!showSource && (
          <>
            {isGraphView && (
              <>
                <div className="main-toolbar-zoom flex items-center gap-1">
                  <IconButton
                    icon={Minus}
                    size="sm"
                    label={t('toolbar.zoomOut')}
                    onClick={() => setGraphZoom(Math.max(0.25, graphZoom - 0.1))}
                  />
                  <button
                    className="main-toolbar-zoom-readout w-12 h-6 text-xs text-text-tertiary tabular-nums text-center rounded-md hover:text-text-secondary hover:bg-surface-2 transition-colors"
                    onClick={() => setGraphZoom(1)}
                    title={t('toolbar.resetZoom')}
                  >
                    {Math.round(graphZoom * 100)}%
                  </button>
                  <IconButton
                    icon={Plus}
                    size="sm"
                    label={t('toolbar.zoomIn')}
                    onClick={() => setGraphZoom(Math.min(3, graphZoom + 0.1))}
                  />
                </div>
                <div className="main-toolbar-wide flex items-center gap-1">
                  <IconButton
                    icon={Maximize2}
                    size="sm"
                    label={t('toolbar.fitToScreen')}
                    onClick={() => {
                      window.dispatchEvent(new Event('graph:fit'))
                    }}
                  />
                  <IconButton
                    icon={Download}
                    size="sm"
                    label={t('toolbar.exportGraphPng')}
                    onClick={() => {
                      window.dispatchEvent(new Event('graph:export-png'))
                    }}
                  />
                </div>
              </>
            )}
            {mainPaneView === 'diff' && (
              <IconButton icon={Plus} size="sm" label={t('toolbar.newComparison')} onClick={handleCompare} />
            )}
            {mainPaneView === 'block' && (
              <IconButton
                icon={Flame}
                size="sm"
                label={t('toolbar.complexityMap')}
                onClick={toggleComplexityMode}
                className={complexityMode ? 'text-semantic-warning bg-semantic-warning/10' : ''}
              />
            )}
            <ToolbarDivider />
          </>
        )}

        <div className="main-toolbar-wide flex items-center gap-1">
          {canReimport && (
            <IconButton
              icon={RefreshCw}
              size="sm"
              label={reimportLabel}
              disabled={reimporting}
              onClick={handleReimport}
            />
          )}
          <IconButton icon={Download} size="sm" label={t('toolbar.exportPdf')} onClick={() => handleExport('pdf')} />
          <IconButton icon={FileCode} size="sm" label={t('toolbar.exportHtml')} onClick={() => handleExport('html')} />
        </div>
        <div className="main-toolbar-narrow items-center">
          <Dropdown
            align="end"
            trigger={<IconButton icon={MoreHorizontal} size="sm" label={t('toolbar.moreActions')} />}
            items={secondaryItems}
          />
        </div>

        <ToolbarDivider />
        <FullscreenButton />
      </div>
    </div>
  )
}
