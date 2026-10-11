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
import {FINDING_CATEGORIES, useAnalysisStore, viewHiddenCategories, type SavedFilterView} from '@/stores/analysisStore'
import {useAuthStore} from '@/stores/authStore'
import type {TriageStatus} from '@/types'
import {categoryColors, categoryBackgrounds} from '@/lib/findingsColors'
import {isTauri} from '@/platform/guards'
import {useConfirm, useToast} from '@/components/shared'
import type {Severity} from '@/types'
import clsx from 'clsx'
import {useDismissable} from '@/hooks/useDismissable'
import {severityTone, triageTone} from '@/lib/severityTone'
import {useState} from 'react'
import {relativeTime} from '@/lib/time'
import {useTranslation} from 'react-i18next'
import {getErrorMessage} from '@/lib/getErrorMessage'

export
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
  snapshots: {id: string; label: string; createdAt: string; bytes: number}[]
  snapshotsLoading: boolean
  onRestoreSnapshot: (snapshotId: string) => void
  onOpenSnapshots: () => void
  sortMode: 'default' | 'severity' | 'count'
  onSetSortMode: (mode: 'default' | 'severity' | 'count') => void
  severityCounts?: Record<Severity, number>
  categories?: readonly string[]
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
  categories = FINDING_CATEGORIES,
  hasFindings,
}: FindingsToolbarProps) {
  const {t} = useTranslation('findings')
  const severityFilter = useAnalysisStore(s => s.severityFilter)
  const hiddenCategories = useAnalysisStore(s => s.hiddenCategories)
  const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
  const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
  const toggleCategory = useAnalysisStore(s => s.toggleCategory)
  const setHiddenCategories = useAnalysisStore(s => s.setHiddenCategories)
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
  const viewsRef = useDismissable(showViews, () => setShowViews(false))
  const [showSort, setShowSort] = useState(false)
  const sortRef = useDismissable(showSort, () => setShowSort(false))
  const [showUndo, setShowUndo] = useState(false)
  const undoRef = useDismissable(showUndo, () => setShowUndo(false))
  const {prompt} = useConfirm()
  const toast = useToast()
  const toggleBaseline = () => {
    const run = baseline ? handleClearBaseline : handleSetBaseline
    const failTitle = baseline ? t('toasts.baselineClearFailed') : t('toasts.baselineSetFailed')
    run().catch(err => toast.error(failTitle, {description: getErrorMessage(err)}))
  }

  const handleSaveView = async () => {
    setShowViews(false)
    const name = await prompt({title: 'Save filter view', message: 'Name this filter view'})
    if (!name?.trim()) return
    saveCurrentView(name.trim(), severityFilter, hiddenCategories)
  }

  const handleApplyView = (view: SavedFilterView) => {
    setShowViews(false)
    setSeverityFilter(new Set(view.severities))
    setHiddenCategories(viewHiddenCategories(view))
  }

  const handleDeleteView = (name: string) => {
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
        {categories.map(cat => {
          return (
            <button
              key={cat}
              onClick={() => toggleCategory(cat)}
              aria-pressed={!hiddenCategories.has(cat)}
              className={clsx(
                'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                !hiddenCategories.has(cat)
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
              title={t('toolbar.triageFilterAria')}
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
            title={t('toolbar.assignedToMe')}
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
        {(severityFilter.size < 3 || hiddenCategories.size > 0 || statusFilter.size < 4 || assignedToMeOnly) && (
          <button
            onClick={() => {
              setSeverityFilter(new Set(['error', 'warning', 'info']))
              setHiddenCategories([])
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
            title={t('toolbar.savedViewsAria')}
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
                {t('toolbar.saveCurrentFilters')}
              </button>
              {savedViews.length > 0 && <div className="border-t border-border-subtle my-0.5" />}
              {savedViews.map(view => (
                <div key={view.name} className="flex items-center group">
                  <button
                    onClick={() => handleApplyView(view)}
                    className="flex-1 min-w-0 text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors"
                  >
                    <span className="text-text-secondary truncate block">{view.name}</span>
                  </button>
                  <button
                    onClick={() => handleDeleteView(view.name)}
                    aria-label={t('toolbar.deleteViewAria', {name: view.name})}
                    className="p-1 mr-1 rounded text-text-disabled hover:text-red-400 opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-all"
                  >
                    <X size={10} />
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
      <button
        onClick={onReanalyze}
        className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors shrink-0"
      >
        Re-analyze
      </button>
      <div className="relative" ref={sortRef}>
        <button
          onClick={() => setShowSort(o => !o)}
          aria-label={t('toolbar.sortAria')}
          aria-haspopup="menu"
          aria-expanded={showSort}
          title={t('toolbar.sortTitle', {label: SORT_OPTIONS.find(o => o.mode === sortMode)?.label ?? 'Rule order'})}
          className={clsx(
            'flex items-center gap-1 text-2xs px-1.5 py-1 rounded transition-colors shrink-0',
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
        title={t('toolbar.compareTitle')}
        aria-label={t('toolbar.compareAria')}
        className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0 disabled:opacity-50"
      >
        <GitCompareArrows size={12} />
      </button>
      <button
        onClick={onToggleDedup}
        disabled={dedupLoading}
        title={dedupActive ? 'Show all findings' : 'Group duplicate findings'}
        aria-label={t('toolbar.toggleGrouping')}
        className={clsx(
          'text-2xs px-1.5 py-1 rounded transition-colors shrink-0 disabled:opacity-50',
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
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
            aria-label={t('toolbar.exportCsvTitle')}
            title={t('toolbar.exportCsvAria')}
          >
            <Download size={12} />
          </button>
          <button
            onClick={onExportHTML}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
            aria-label={t('toolbar.exportHtmlTitle')}
            title={t('toolbar.exportHtmlAria')}
          >
            <FileText size={12} />
          </button>
          <button
            onClick={onExportSARIF}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
            aria-label={t('toolbar.exportSarifTitle')}
            title={t('toolbar.exportSarifAria')}
          >
            <FileCode size={12} />
          </button>
          <button
            onClick={onExportJUnit}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
            aria-label={t('toolbar.exportJunitTitle')}
            title={t('toolbar.exportJunitAria')}
          >
            <ShieldCheck size={12} />
          </button>
        </>
      )}
      {cloudMode && (
        <div className="flex items-center gap-1 shrink-0 border-l border-border-subtle pl-1.5 ml-0.5">
          {baselineNewCount !== null && baselineNewCount > 0 && (
            <span className="text-2xs font-medium text-semantic-warning bg-semantic-warning/10 px-1.5 py-0.5 rounded-full">
              {baselineNewCount} new
            </span>
          )}
          <button
            onClick={toggleBaseline}
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
            title={t('toolbar.shareLinkTitle')}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors"
          >
            <Share2 size={12} />
          </button>
          <div className="relative" ref={undoRef}>
            <button
              onClick={() => {
                onOpenSnapshots()
                setShowUndo(o => !o)
              }}
              title={t('toolbar.undoTitle')}
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
                  <div className="px-2.5 py-1.5 text-2xs text-text-tertiary">{t('toolbar.noSnapshots')}</div>
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
