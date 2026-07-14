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
} from 'lucide-react'
import {useAnalysisStore, type FindingCategory, type SavedFilterView} from '@/stores/analysisStore'
import {categoryColors, categoryBackgrounds} from '@/lib/findingsColors'
import {isTauri} from '@/platform/guards'
import type {Severity} from '@/types'
import clsx from 'clsx'
import {useState} from 'react'

const ALL_CATEGORIES: FindingCategory[] = ['Security', 'Reliability', 'Performance', 'Style', 'Logic']

// FindingsToolbar is the filter + actions bar above the findings list: severity
// and category filter chips, re-analyze, sort cycle, diff/dedup toggles, and
// CSV/HTML export. Extracted from FindingsTab so the filter state and chip
// rendering are isolated from orchestration.
//
// Filter state is read directly from the analysis store (it is the single source
// of truth); the one-shot actions are passed in because they are composed with
// toasts and sibling state in FindingsTab.
export interface FindingsToolbarProps {
  onReanalyze: () => void
  onShowDiff: () => void
  diffLoading: boolean
  onToggleDedup: () => void
  dedupActive: boolean
  dedupLoading: boolean
  onExportCSV: () => void
  onExportHTML: () => void
  onExportSARIF: () => void
  onShare: () => void
  sortMode: 'default' | 'severity' | 'count'
  onCycleSortMode: () => void
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
  onShare,
  sortMode,
  onCycleSortMode,
  hasFindings,
}: FindingsToolbarProps) {
  const severityFilter = useAnalysisStore(s => s.severityFilter)
  const categoryFilter = useAnalysisStore(s => s.categoryFilter)
  const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
  const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
  const toggleCategoryFilter = useAnalysisStore(s => s.toggleCategoryFilter)
  const setCategoryFilter = useAnalysisStore(s => s.setCategoryFilter)
  const baseline = useAnalysisStore(s => s.baseline)
  const baselineNewCount = useAnalysisStore(s => s.baselineNewCount)
  const handleSetBaseline = useAnalysisStore(s => s.handleSetBaseline)
  const handleClearBaseline = useAnalysisStore(s => s.handleClearBaseline)
  const cloudMode = !isTauri()
  const savedViews = useAnalysisStore(s => s.savedViews)
  const saveCurrentView = useAnalysisStore(s => s.saveCurrentView)
  const deleteSavedView = useAnalysisStore(s => s.deleteSavedView)
  const [showViews, setShowViews] = useState(false)

  const handleSaveView = () => {
    setShowViews(false)
    const name = window.prompt('Name this filter view')
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
    <div className="px-3 py-1.5 flex items-center justify-between border-b border-border-subtle gap-2">
      <div className="flex items-center gap-1">
        {[
          {s: 'error' as Severity, label: 'Errors', color: 'text-red-500', bg: 'bg-red-500/10'},
          {s: 'warning' as Severity, label: 'Warnings', color: 'text-amber-500', bg: 'bg-amber-500/10'},
          {s: 'info' as Severity, label: 'Info', color: 'text-blue-500', bg: 'bg-blue-500/10'},
        ].map(({s, label, color, bg}) => (
          <button
            key={s}
            onClick={() => toggleSeverityFilter(s)}
            className={clsx(
              'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
              severityFilter.has(s)
                ? `${bg} ${color} border-transparent`
                : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
            )}
          >
            {label}
          </button>
        ))}
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
        {(severityFilter.size < 3 || categoryFilter.size < 5) && (
          <button
            onClick={() => {
              setSeverityFilter(new Set(['error', 'warning', 'info']))
              useAnalysisStore.getState().setCategoryFilter(new Set(ALL_CATEGORIES))
            }}
            className="text-2xs text-text-tertiary hover:text-text-secondary transition-colors ml-1"
          >
            All
          </button>
        )}
        <div className="relative">
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
      <button
        onClick={onCycleSortMode}
        aria-label="Change findings sort order"
        title={
          sortMode === 'severity'
            ? 'Sort: by severity — click for count'
            : sortMode === 'count'
              ? 'Sort: by count — click for rule order'
              : 'Sort: rule order — click for severity'
        }
        className={clsx(
          'text-2xs px-1.5 py-1 rounded transition-colors flex-shrink-0',
          sortMode !== 'severity'
            ? 'text-brand-400 bg-brand-500/10'
            : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
        )}
      >
        <ArrowUpDown size={12} />
      </button>
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
        aria-label="Toggle duplicate grouping"
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
        </>
      )}
      {cloudMode && (
        <div className="flex items-center gap-1 flex-shrink-0 border-l border-border-subtle pl-1.5 ml-0.5">
          {baselineNewCount !== null && baselineNewCount > 0 && (
            <span className="text-2xs font-medium text-amber-400 bg-amber-500/10 px-1.5 py-0.5 rounded-full">
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
        </div>
      )}
    </div>
  )
}
