import {GitCompareArrows, Layers, ArrowUpDown, Download, FileText} from 'lucide-react'
import {useAnalysisStore, type FindingCategory} from '@/stores/analysisStore'
import {categoryColors, categoryBackgrounds} from '@/lib/findingsColors'
import type {Severity} from '@/types'
import clsx from 'clsx'

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
    sortMode,
    onCycleSortMode,
    hasFindings,
}: FindingsToolbarProps) {
    const severityFilter = useAnalysisStore(s => s.severityFilter)
    const categoryFilter = useAnalysisStore(s => s.categoryFilter)
    const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
    const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
    const toggleCategoryFilter = useAnalysisStore(s => s.toggleCategoryFilter)

    return (
        <div className="px-3 py-1.5 flex items-center justify-between border-b border-border-subtle gap-2">
            <div className="flex items-center gap-1">
                {([
                    {s: 'error' as Severity, label: 'Errors', color: 'text-red-500', bg: 'bg-red-500/10'},
                    {s: 'warning' as Severity, label: 'Warnings', color: 'text-amber-500', bg: 'bg-amber-500/10'},
                    {s: 'info' as Severity, label: 'Info', color: 'text-blue-500', bg: 'bg-blue-500/10'},
                ]).map(({s, label, color, bg}) => (
                    <button
                        key={s}
                        onClick={() => toggleSeverityFilter(s)}
                        className={clsx(
                            'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                            severityFilter.has(s)
                                ? `${bg} ${color} border-transparent`
                                : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary'
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
                                    : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary'
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
                title={sortMode === 'severity' ? 'Sort: by severity — click for count' : sortMode === 'count' ? 'Sort: by count — click for rule order' : 'Sort: rule order — click for severity'}
                className={clsx(
                    'text-2xs px-1.5 py-1 rounded transition-colors flex-shrink-0',
                    sortMode !== 'severity'
                        ? 'text-brand-400 bg-brand-500/10'
                        : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3'
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
                        : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3'
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
                </>
            )}
        </div>
    )
}
