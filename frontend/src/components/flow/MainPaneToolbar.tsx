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
} from 'lucide-react'
import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import SegmentedControl from '@/components/shared/SegmentedControl'
import IconButton from '@/components/shared/IconButton'
import {useUIStore, isSystemView} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {useAuthStore} from '@/stores/authStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {exportApi, flowApi, analysisApi} from '@/api'
import {useToast} from '@/components/shared/Toast'
import type {FlowDiff, AnalysisReport} from '@/types'

type SystemView = 'home' | 'dashboard' | 'library' | 'profile' | 'admin'
const SYSTEM_VIEW_TITLES: Record<SystemView, string> = {
  home: 'Welcome',
  dashboard: 'Analysis Dashboard',
  library: 'Cloud Library',
  profile: 'User Profile',
  admin: 'Admin',
}

export default function MainPaneToolbar() {
  const mainPaneView = useUIStore(s => s.mainPaneView)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const complexityMode = useUIStore(s => s.complexityMode)
  const toggleComplexityMode = useUIStore(s => s.toggleComplexityMode)
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const setActiveDiff = useUIStore(s => s.setActiveDiff)
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

  const toast = useToast()
  const userRole = useAuthStore(s => s.user?.role)

  if (isSystemView(mainPaneView)) {
    const systemOptions: {value: SystemView; label: string; icon: typeof LayoutDashboard}[] = [
      {value: 'home', label: t('nav.home'), icon: LayoutDashboard},
      {value: 'dashboard', label: t('nav.analytics'), icon: BarChart3},
      {value: 'library', label: t('nav.library'), icon: Cloud},
      {value: 'profile', label: t('nav.profile'), icon: User},
    ]
    if (userRole === 'admin') systemOptions.push({value: 'admin', label: t('nav.admin'), icon: Shield})
    return (
      <div className="flex items-center h-12 px-4 border-b border-border-default bg-surface-1 gap-4">
        <SegmentedControl
          value={mainPaneView as SystemView}
          onChange={v => setMainPaneView(v)}
          options={systemOptions}
          size="sm"
        />
        <div className="flex-1 flex items-center">
          <span className="text-sm font-medium text-text-primary">
            {SYSTEM_VIEW_TITLES[mainPaneView as SystemView]}
          </span>
        </div>
        <IconButton
          icon={Expand}
          size="sm"
          label={t('toolbar.fullscreen')}
          onClick={() => {
            try {
              void window.document.documentElement.requestFullscreen()
            } catch {
              /* fullscreen not supported */
            }
          }}
        />
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

  return (
    <div className="flex items-center h-12 px-4 border-b border-border-default bg-surface-1 gap-4">
      <div className="flex items-center gap-1">
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
      </div>

      <div className="w-px h-6 bg-border-subtle mx-1" />

      <SegmentedControl
        value={mainPaneView}
        onChange={setMainPaneView}
        options={[
          {value: 'block' as const, label: 'Block', icon: List},
          {value: 'graph' as const, label: 'Graph', icon: Network},
          {value: 'local-map' as const, label: 'Local Map', icon: MapPin},
          {value: 'map' as const, label: 'Map', icon: Map},
          {value: 'diff' as const, label: 'Diff', icon: History},
        ]}
        size="sm"
      />

      {/* The center breadcrumb copy is GONE (U5a.3): the interactive
          Breadcrumbs bar renders directly under the tab strip — showing the
          same path twice in one pane was chrome, not information. The diff
          label stays (it replaces the editor view, not a duplicate). */}
      <div className="flex-1 flex items-center justify-center">
        {mainPaneView === 'diff' && (
          <div className="text-sm font-medium text-brand-500 flex items-center gap-2">
            <History size={16} />
            Version Comparison
          </div>
        )}
      </div>

      <div className="flex items-center gap-1">
        {(mainPaneView === 'graph' || mainPaneView === 'map' || mainPaneView === 'local-map') && (
          <>
            <IconButton
              icon={Minus}
              size="sm"
              label={t('toolbar.zoomOut')}
              onClick={() => setGraphZoom(Math.max(0.25, graphZoom - 0.1))}
            />
            <button
              className="w-12 h-7 text-xs text-text-tertiary tabular-nums text-center hover:text-text-secondary"
              onClick={() => setGraphZoom(1)}
            >
              {Math.round(graphZoom * 100)}%
            </button>
            <IconButton
              icon={Plus}
              size="sm"
              label={t('toolbar.zoomIn')}
              onClick={() => setGraphZoom(Math.min(3, graphZoom + 0.1))}
            />
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
        <IconButton
          icon={RefreshCw}
          size="sm"
          label={reimporting ? t('toolbar.reimporting') : t('toolbar.reimport')}
          disabled={reimporting}
          onClick={handleReimport}
        />
        <IconButton
          icon={Expand}
          size="sm"
          label={t('toolbar.fullscreen')}
          onClick={() => {
            try {
              void window.document.documentElement.requestFullscreen()
            } catch {
              /* fullscreen not supported */
            }
          }}
        />
        <IconButton icon={Download} size="sm" label={t('toolbar.exportPdf')} onClick={() => handleExport('pdf')} />
        <IconButton icon={Code2} size="sm" label={t('toolbar.exportHtml')} onClick={() => handleExport('html')} />
      </div>
    </div>
  )
}
