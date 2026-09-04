import {
  GitCompareArrows,
  Layers,
  ArrowUpDown,
  Download,
  FileText,
  ShieldCheck,
  FileCode,
  Bookmark,
  X,
  Share2,
  Undo2,
} from 'lucide-react'
import {useAnalysisStore, type FindingCategory, type SavedFilterView} from '@/stores/analysisStore'
import {useAuthStore} from '@/stores/authStore'
import type {TriageStatus} from '@/types'
import {categoryColors, categoryBackgrounds} from '@/lib/findingsColors'
import {isTauri} from '@/platform/guards'
import {useConfirm} from '@/components/shared'
import type {Severity} from '@/types'
import clsx from 'clsx'
import {useDismissable} from '@/hooks/useDismissable'
import {severityTone, triageTone} from '@/lib/severityTone'
import {useState} from 'react'
import {relativeTime} from '@/lib/time'
import {useTranslation} from 'react-i18next'

const ALL_CATEGORIES: FindingCategory[] = ['Security', 'Reliability', 'Performance', 'Style', 'Logic']

// FindingsToolbar is the filter + actions bar above the findings list: severity
// and category filter chips, re-analyze, sort cycle, diff/dedup toggles, and
// CSV/HTML export. Extracted from FindingsTab so the filter state and chip
// rendering are isolated from orchestration.
//
// Filter state is read directly from the analysis store (it is the single source
// of truth); the one-shot actions are passed in because they are composed with
// toasts and sibling state in FindingsTab.
export // Chip labels kept beside their tone consumers; counts render inline.
const SEVERITY_LABELS: Record<Severity, string> = {error: 'Errors', warning: 'Warnings', info: 'Info'}
const STATUS_LABELS: Record<TriageStatus, string> = {
  open: 'Open',
  acknowledged: 'Ack',
  in_progress: 'Active',
  resolved: 'Resolved',
  suppressed: 'Suppressed',
}

const SORT_OPTIONS = [
  {mode: 'severity', label: 'By severity'},
  {mode: 'count', label: 'By count'},
  {mode: 'default', label: 'Rule order'},
] as const

interface FindingsToolbarProps {
  onReanalyze: () => void
  onShowDiff: () => void
  diffLoading: boolean
  onToggleDedup: () => void
  dedupActive: boolean
  dedupLoading: boolean
  onExportCSV: () => void
  onExportHTML: () => void
  onExportSARIF: () => void
  onExportJUnit: () => void
  onShare: () => void
  // Undo (R1-2): snapshots the server captured before every fix/batch/save.
  snapshots: {id: string; label: string; createdAt: string; bytes: number}[]
  snapshotsLoading: boolean
  onRestoreSnapshot: (snapshotId: string) => void
  onOpenSnapshots: () => void
  sortMode: 'default' | 'severity' | 'count'
  onSetSortMode: (mode: 'default' | 'severity' | 'count') => void
  // Live per-severity counts of the CURRENT (non-severity-filtered) set —
  // shown on the chips so toggling is an informed choice (U2.3).
  severityCounts?: Record<Severity, number>
  hasFindings: boolean
}

export default function FindingsToolbar({
  onReanalyze,
  onShowDiff,
  diffLoading,
  onToggleDedup,
  dedupActive,
  dedupLoading,
  onExportCSV,
  onExportHTML,
  onExportSARIF,
  onExportJUnit,
  onShare,
  snapshots,
  snapshotsLoading,
  onRestoreSnapshot,
  onOpenSnapshots,
  sortMode,
  onSetSortMode,
  severityCounts,
  hasFindings,
}: FindingsToolbarProps) {
  const {t} = useTranslation('findings')
  const severityFilter = useAnalysisStore(s => s.severityFilter)
  const categoryFilter = useAnalysisStore(s => s.categoryFilter)
  const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
  const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
  const toggleCategoryFilter = useAnalysisStore(s => s.toggleCategoryFilter)
  const setCategoryFilter = useAnalysisStore(s => s.setCategoryFilter)
  const statusFilter = useAnalysisStore(s => s.statusFilter)
  const toggleStatusFilter = useAnalysisStore(s => s.toggleStatusFilter)
  const setStatusFilter = useAnalysisStore(s => s.setStatusFilter)
  const assignedToMeOnly = useAnalysisStore(s => s.assignedToMeOnly)
  const toggleAssignedToMe = useAnalysisStore(s => s.toggleAssignedToMe)
  const currentUserId = useAuthStore(s => s.user?.id ?? '')
  const baseline = useAnalysisStore(s => s.baseline)
  const baselineNewCount = useAnalysisStore(s => s.baselineNewCount)
  const handleSetBaseline = useAnalysisStore(s => s.handleSetBaseline)
  const handleClearBaseline = useAnalysisStore(s => s.handleClearBaseline)
  const cloudMode = !isTauri()
  const savedViews = useAnalysisStore(s => s.savedViews)
  const saveCurrentView = useAnalysisStore(s => s.saveCurrentView)
  const deleteSavedView = useAnalysisStore(s => s.deleteSavedView)
  const [showViews, setShowViews] = useState(false)
  // Popover dismissal contract (U1.5): outside click + Escape close both
  // toolbar popovers (they previously stuck open until re-clicked).
  const viewsRef = useDismissable(showViews, () => setShowViews(false))
  const [showSort, setShowSort] = useState(false)
  const sortRef = useDismissable(showSort, () => setShowSort(false))
  const [showUndo, setShowUndo] = useState(false)
  const undoRef = useDismissable(showUndo, () => setShowUndo(false))
  const {prompt} = useConfirm()

  const handleSaveView = async () => {
    setShowViews(false)
    const name = await prompt({title: 'Save filter view', message: 'Name this filter view'})
    if (!name?.trim()) return
    saveCurrentView(name.trim(), severityFilter, categoryFilter)
  }

  const handleApplyView = (view: SavedFilterView) => {
    setShowViews(false)
    setSeverityFilter(new Set(view.severities))
    setCategoryFilter(new Set(view.categories as FindingCategory[]))
  }

  const handleDeleteView = (e: React.MouseEvent, name: string) => {
    e.stopPropagation()
    deleteSavedView(name)
  }

  return (
    <div className="px-3 py-1.5 flex items-center justify-between border-b border-border-subtle gap-2 flex-wrap">
      <div className="flex items-center gap-1">
        {(['error', 'warning', 'info'] as Severity[]).map(s => {
          const tone = severityTone(s)
          const count = severityCounts?.[s]
          return (
            <button
              key={s}
              onClick={() => toggleSeverityFilter(s)}
              className={clsx(
                'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                severityFilter.has(s)
                  ? `${tone.bg} ${tone.text} border-transparent`
                  : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
              )}
            >
              {SEVERITY_LABELS[s]}
              {count !== undefined && count > 0 && <span className="ml-1 font-normal opacity-70">{count}</span>}
            </button>
          )
        })}
        <span className="text-border-subtle mx-0.5">|</span>
        {ALL_CATEGORIES.map(cat => {
          return (
            <button
              key={cat}
              onClick={() => toggleCategoryFilter(cat)}
              className={clsx(
                'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                categoryFilter.has(cat)
                  ? `${categoryBackgrounds[cat] ?? 'bg-surface-3'} ${categoryColors[cat] ?? 'text-text-tertiary'} border-transparent`
                  : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
              )}
            >
              {cat}
            </button>
          )
        })}
        <span className="text-border-subtle mx-0.5">|</span>
        {(['open', 'acknowledged', 'in_progress', 'resolved'] as TriageStatus[]).map(s => {
          const tone = triageTone(s)
          const label = STATUS_LABELS[s]
          return (
            <button
              key={s}
              onClick={() => toggleStatusFilter(s)}
              aria-pressed={statusFilter.has(s)}
              title="Filter by triage status"
              className={clsx(
                'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                statusFilter.has(s)
                  ? `${tone.bg} ${tone.text} border-transparent`
                  : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
              )}
            >
              {label}
            </button>
          )
        })}
        {currentUserId && (
          <button
            onClick={toggleAssignedToMe}
            aria-pressed={assignedToMeOnly}
            title="Show only findings assigned to me"
            className={clsx(
              'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
              assignedToMeOnly
                ? 'bg-brand-500/10 text-brand-400 border-transparent'
                : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
            )}
          >
            Mine
          </button>
        )}
        {(severityFilter.size < 3 || categoryFilter.size < 5 || statusFilter.size < 4 || assignedToMeOnly) && (
          <button
            onClick={() => {
              setSeverityFilter(new Set(['error', 'warning', 'info']))
              useAnalysisStore.getState().setCategoryFilter(new Set(ALL_CATEGORIES))
              setStatusFilter(new Set(['open', 'acknowledged', 'in_progress', 'resolved']))
              if (assignedToMeOnly) toggleAssignedToMe()
            }}
            className="text-2xs text-text-tertiary hover:text-text-secondary transition-colors ml-1"
          >
            All
          </button>
        )}
        <div className="relative" ref={viewsRef}>
          <button
            onClick={() => setShowViews(s => !s)}
            className={clsx(
              'text-2xs px-1.5 py-0.5 rounded-full border transition-all duration-fast ml-1',
              showViews
                ? 'bg-brand-500/10 text-brand-400 border-transparent'
                : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
            )}
            title="Saved filter views"
          >
            <Bookmark size={11} />
          </button>
          {showViews && (
            <div className="absolute left-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-44">
              <button
                onClick={handleSaveView}
                className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1.5 hover:bg-surface-3 transition-colors text-brand-400 font-medium"
              >
                <Bookmark size={10} />
                Save current filters…
              </button>
              {savedViews.length > 0 && <div className="border-t border-border-subtle my-0.5" />}
              {savedViews.map(view => (
                <button
                  key={view.name}
                  onClick={() => handleApplyView(view)}
                  className="flex items-center justify-between w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors group"
                >
                  <span className="text-text-secondary truncate">{view.name}</span>
                  <span
                    onClick={e => handleDeleteView(e, view.name)}
                    className="opacity-0 group-hover:opacity-100 text-text-disabled hover:text-red-400 transition-all ml-2"
                  >
                    <X size={10} />
                  </span>
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
      <button
        onClick={onReanalyze}
        className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors flex-shrink-0"
      >
        Re-analyze
      </button>
      {/* Sort: an explicit 3-option menu (U2.3) — the old cycle button hid
          its state in a tooltip and silently changed meaning per click. */}
      <div className="relative" ref={sortRef}>
        <button
          onClick={() => setShowSort(o => !o)}
          aria-label="Change findings sort order"
          aria-haspopup="menu"
          aria-expanded={showSort}
          title={`Sort: ${SORT_OPTIONS.find(o => o.mode === sortMode)?.label ?? 'Rule order'}`}
          className={clsx(
            'flex items-center gap-1 text-2xs px-1.5 py-1 rounded transition-colors flex-shrink-0',
            sortMode !== 'severity'
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
          )}
        >
          <ArrowUpDown size={12} />
          <span className="hidden sm:inline">{SORT_OPTIONS.find(o => o.mode === sortMode)?.label}</span>
        </button>
        {showSort && (
          <div
            role="menu"
            className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-36"
          >
            {SORT_OPTIONS.map(o => (
              <button
                key={o.mode}
                role="menuitemradio"
                aria-checked={sortMode === o.mode}
                onClick={() => {
                  onSetSortMode(o.mode)
                  setShowSort(false)
                }}
                className="flex w-full items-center justify-between gap-2 text-left text-2xs px-2.5 py-1.5 hover:bg-surface-3 transition-colors text-text-secondary"
              >
                <span>{o.label}</span>
                {sortMode === o.mode && <span className="text-brand-400">✓</span>}
              </button>
            ))}
          </div>
        )}
      </div>
      <button
        onClick={onShowDiff}
        disabled={diffLoading}
        title="Compare with previous run"
        aria-label="Compare findings with previous analysis run"
        className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0 disabled:opacity-50"
      >
        <GitCompareArrows size={12} />
      </button>
      <button
        onClick={onToggleDedup}
        disabled={dedupLoading}
        title={dedupActive ? 'Show all findings' : 'Group duplicate findings'}
        aria-label={t('toolbar.toggleGrouping')}
        className={clsx(
          'text-2xs px-1.5 py-1 rounded transition-colors flex-shrink-0 disabled:opacity-50',
          dedupActive
            ? 'text-brand-400 bg-brand-500/10'
            : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
        )}
      >
        <Layers size={12} />
      </button>
      {hasFindings && (
        <>
          <button
            onClick={onExportCSV}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
            aria-label="Export findings as CSV"
            title="Export as CSV"
          >
            <Download size={12} />
          </button>
          <button
            onClick={onExportHTML}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
            aria-label="Export findings as HTML"
            title="Export as HTML"
          >
            <FileText size={12} />
          </button>
          <button
            onClick={onExportSARIF}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
            aria-label="Export findings as SARIF"
            title="Export as SARIF (GitHub Code Scanning)"
          >
            <FileCode size={12} />
          </button>
          <button
            onClick={onExportJUnit}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
            aria-label="Export findings as JUnit XML"
            title="Export as JUnit XML (CI dashboards)"
          >
            <ShieldCheck size={12} />
          </button>
        </>
      )}
      {cloudMode && (
        <div className="flex items-center gap-1 flex-shrink-0 border-l border-border-subtle pl-1.5 ml-0.5">
          {baselineNewCount !== null && baselineNewCount > 0 && (
            <span className="text-2xs font-medium text-semantic-warning bg-semantic-warning/10 px-1.5 py-0.5 rounded-full">
              {baselineNewCount} new
            </span>
          )}
          <button
            onClick={() => (baseline ? handleClearBaseline() : handleSetBaseline())}
            title={
              baseline ? 'Clear baseline (track all findings again)' : 'Set current findings as baseline (ratchet)'
            }
            className={clsx(
              'text-2xs px-1.5 py-1 rounded transition-colors',
              baseline
                ? 'text-brand-400 hover:text-brand-300 hover:bg-brand-500/10'
                : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
            )}
          >
            <ShieldCheck size={12} />
          </button>
          <button
            onClick={onShare}
            title="Create read-only share link"
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors"
          >
            <Share2 size={12} />
          </button>
          {/* Undo (R1-2): restore a pre-fix/pre-save snapshot. The server
              captures the source before every destructive mutation; this
              lists this session's ring, newest last. */}
          <div className="relative" ref={undoRef}>
            <button
              onClick={() => {
                onOpenSnapshots()
                setShowUndo(o => !o)
              }}
              title="Undo a fix or save (restore a snapshot)"
              className={clsx(
                'text-2xs px-1.5 py-1 rounded transition-colors',
                showUndo
                  ? 'text-brand-400 bg-surface-3'
                  : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
              )}
            >
              <Undo2 size={12} />
            </button>
            {showUndo && (
              <div className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-56 max-h-64 overflow-y-auto">
                {snapshotsLoading ? (
                  <div className="px-2.5 py-1.5 text-2xs text-text-tertiary">Loading…</div>
                ) : snapshots.length === 0 ? (
                  <div className="px-2.5 py-1.5 text-2xs text-text-tertiary">
                    No snapshots yet — one is captured before every fix, batch fix, or source save.
                  </div>
                ) : (
                  [...snapshots].reverse().map(snap => (
                    <button
                      key={snap.id}
                      onClick={() => {
                        setShowUndo(false)
                        onRestoreSnapshot(snap.id)
                      }}
                      className="flex w-full items-center justify-between gap-3 text-left text-2xs px-2.5 py-1.5 hover:bg-surface-3 transition-colors"
                    >
                      <span className="text-text-secondary truncate">{snap.label}</span>
                      <span className="text-text-tertiary/70 shrink-0">{relativeTime(snap.createdAt)}</span>
                    </button>
                  ))
                )}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
