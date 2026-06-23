import {useCallback, useEffect, useMemo, useState} from 'react'
import {Download, FileText, Search, GitCompareArrows, Layers, ArrowUpDown} from 'lucide-react'
import {analysisApi} from '@/api'
import {useAnalysisStore, findingKey, type FindingCategory} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {EmptyState, Spinner, useToast} from '@/components/shared'
import {categoryColors, categoryBackgrounds} from '@/lib/findingsColors'
import FindingsSummary from './FindingsSummary'
import FindingsList from './FindingsList'
import AnalysisDiffView from './AnalysisDiffView'
import {exportFindingsCSV, exportFindingsHTML} from '@/lib/findingsExport'
import {buildBlockLookup, type BlockLookup} from '@/lib/tree'
import type {AnalysisDiff, Finding, Severity, AnalysisReport} from '@/types'
import clsx from 'clsx'

export default function FindingsTab() {
  const doc = useFlowStore(s => s.document)
  const report = useAnalysisStore(s => doc ? s.reports.get(doc.id) : undefined)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const progress = useAnalysisStore(s => s.progress)
  const setReport = useAnalysisStore(s => s.setReport)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const setProgress = useAnalysisStore(s => s.setProgress)
  const severityFilter = useAnalysisStore(s => s.severityFilter)
  const categoryFilter = useAnalysisStore(s => s.categoryFilter)
  const appendMessage = useChatStore(s => s.appendMessage)
  const createThread = useChatStore(s => s.createThread)
  const updateThread = useChatStore(s => s.updateThread)
  const switchThread = useChatStore(s => s.switchThread)
  const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
  const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
  const toggleCategoryFilter = useAnalysisStore(s => s.toggleCategoryFilter)
  const allCategories: FindingCategory[] = ['Security', 'Reliability', 'Performance', 'Style', 'Logic']
  const findingSearch = useAnalysisStore(s => s.findingSearch)
  const setFindingSearch = useAnalysisStore(s => s.setFindingSearch)
  const setInspectorTab = useUIStore(s => s.setInspectorTab)
  const suppressedKeys = useAnalysisStore(s => s.suppressedKeys)
  const loadSuppressions = useAnalysisStore(s => s.loadSuppressions)
  const toast = useToast()
  const [diff, setDiff] = useState<AnalysisDiff | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)
  const [dedupGroups, setDedupGroups] = useState<Map<string, number> | null>(null)
  const [dedupLoading, setDedupLoading] = useState(false)
  const [sortMode, setSortMode] = useState<'default' | 'severity' | 'count'>('severity')
  const cycleSortMode = useCallback(() => {
    setSortMode(m => m === 'severity' ? 'count' : m === 'count' ? 'default' : 'severity')
  }, [])

  // Leave the diff view when switching documents or after a fresh analysis.
  useEffect(() => {
    setDiff(null)
    setDedupGroups(null)
  }, [doc?.id, report?.generatedAt])

  // Pull persisted, team-shared suppressions for the active flow (cloud mode;
  // a no-op on desktop). Keeps suppressions in sync across users/sessions.
  useEffect(() => {
    if (doc?.id) void loadSuppressions(doc.id)
  }, [doc?.id, loadSuppressions])

  // Build a blockId → label index once per document so the findings list can
  // resolve each row's block name in O(1), instead of walking the whole block
  // tree per finding (which made a large report's render O(findings × blocks)).
  const blockLookup = useMemo<BlockLookup>(() => doc ? buildBlockLookup(doc) : new Map(), [doc])

  const findings = useMemo(() => {
    if (!report) return []
    const q = findingSearch.toLowerCase()
    return report.findings.filter(f => {
      if (dedupGroups && !dedupGroups.has(f.id)) return false
      if (!severityFilter.has(f.severity)) return false
      if (f.category && !categoryFilter.has(f.category as FindingCategory)) return false
      if (suppressedKeys.has(findingKey(f))) return false
      if (q && !f.title.toLowerCase().includes(q) && !f.description.toLowerCase().includes(q) && !f.ruleId.toLowerCase().includes(q)) return false
      return true
    })
  }, [report, severityFilter, categoryFilter, findingSearch, suppressedKeys, dedupGroups])

  const suppressedCount = useMemo(() => {
    if (!report) return 0
    let count = 0
    for (const f of report.findings) {
      if (suppressedKeys.has(findingKey(f))) count++
    }
    return count
  }, [report, suppressedKeys])

  const handleAnalyze = useCallback(async () => {
    if (!doc) return
    const gen = beginAnalyzing()
    setProgress({current: 0, total: 0, ruleName: ''})

    try {
      const r = await analysisApi.analyzeFlow()
      if (r) {
        setReport(doc.id, r as AnalysisReport)
      }
    } catch (err) {
      toast.error('Analysis failed', {description: String(err)})
    } finally {
      // Read the latest gen from the store rather than a captured render-time
      // copy: beginAnalyzing() bumps the gen during this same handler, so a
      // closure-captured value is always stale and the equality check would
      // never pass, leaving isAnalyzing stuck true.
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
    }
  }, [doc, toast, setReport, setAnalyzing, beginAnalyzing, setProgress])

  const handleFixWithAI = useCallback((finding: Finding) => {
    if (!doc) return
    const threadId = createThread(doc.id)
    updateThread(threadId, {
      title: `Fix: ${finding.title}`,
      contextBlockId: finding.blockId,
      // Fix-with-AI benefits most from grounding, so enable the read-only tool
      // loop for this thread (no-op on providers that don't support tools).
      useTools: true,
    })
    // Ground the AI with everything the analyzer knows about this finding,
    // including its machine-generated fix hint when one exists.
    const parts = [
      `Help me fix this issue: **${finding.title}**`,
      finding.description,
      finding.suggestion ? `Suggestion: ${finding.suggestion}` : '',
      finding.autoFixHint ? `Analyzer fix hint:\n\`\`\`\n${finding.autoFixHint}\n\`\`\`` : '',
      `Rule: \`${finding.ruleId}\` · Severity: ${finding.severity} · Block: \`${finding.blockId}\``,
    ].filter(Boolean)
    appendMessage(threadId, {
      id: crypto.randomUUID(),
      role: 'user',
      content: parts.join('\n\n'),
      timestamp: new Date().toISOString(),
      contextBlockId: finding.blockId,
    })
    switchThread(threadId)
    setInspectorTab('ai')
  }, [appendMessage, createThread, updateThread, switchThread, setInspectorTab, doc])

  const handleShowDiff = useCallback(async () => {
    setDiffLoading(true)
    try {
      const d = await analysisApi.getDiff()
      setDiff(d)
    } catch (err) {
      toast.error('Diff failed: ' + (err as Error).message)
    } finally {
      setDiffLoading(false)
    }
  }, [setDiffLoading, setDiff, toast])

  const handleToggleDedup = useCallback(async () => {
    if (dedupGroups) {
      setDedupGroups(null)
      return
    }
    setDedupLoading(true)
    try {
      const result = await analysisApi.deduplicate()
      const map = new Map<string, number>()
      for (const f of result.deduplicated) {
        map.set(f.id, 0)
      }
      setDedupGroups(map)
    } catch (err) {
      toast.error('Deduplicate failed: ' + (err as Error).message)
    } finally {
      setDedupLoading(false)
    }
  }, [dedupGroups, toast, setDedupGroups, setDedupLoading])

  const handleExportCSV = useCallback(() => {
    if (!report) return
    exportFindingsCSV(report, doc?.id ?? 'report')
  }, [report, doc])

  const handleExportHTML = useCallback(async () => {
    if (!doc) return
    try {
      await exportFindingsHTML(doc.id)
    } catch (err) {
      toast.error('HTML export failed: ' + (err as Error).message)
    }
  }, [doc, toast])

  if (!doc) {
    return (
      <div className="flex items-center justify-center h-full text-sm text-text-tertiary p-4 text-center">
        Load a flow to run analysis
      </div>
    )
  }

  if (!report && !isAnalyzing) {
    return (
      <div className="flex flex-col items-center justify-center h-full gap-3 p-4">
        <span className="text-sm text-text-tertiary">No analysis run yet</span>
        <button
          onClick={handleAnalyze}
          className="px-4 py-2 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors"
        >
          Run Analysis
        </button>
      </div>
    )
  }

  if (isAnalyzing) {
    const pct = progress.total > 0 ? Math.round(progress.current / progress.total * 100) : 0
    return (
      <div className="flex flex-col items-center justify-center h-full gap-3 p-4">
        <Spinner size={24} />
        <span className="text-sm text-text-secondary tabular-nums">
          Analyzing... {pct}% ({progress.ruleName})
        </span>
      </div>
    )
  }

  // Unreachable given the guards above (report is set whenever we get here),
  // but this narrows the type so the render below needs no non-null assertions.
  if (!report) return null

  return (
    <div className="flex flex-col h-full">
      <FindingsSummary stats={report.stats} durationMs={report.durationMs} healthScore={report.metrics?.healthScore} />
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
          {allCategories.map(cat => {
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
                useAnalysisStore.getState().setCategoryFilter(new Set(allCategories))
              }}
              className="text-2xs text-text-tertiary hover:text-text-secondary transition-colors ml-1"
            >
              All
            </button>
          )}
        </div>
        <button
          onClick={handleAnalyze}
          className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors flex-shrink-0"
        >
          Re-analyze
        </button>
        <button
          onClick={cycleSortMode}
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
          onClick={handleShowDiff}
          disabled={diffLoading}
          title="Compare with previous run"
          aria-label="Compare findings with previous analysis run"
          className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0 disabled:opacity-50"
        >
          <GitCompareArrows size={12} />
        </button>
        <button
          onClick={handleToggleDedup}
          disabled={dedupLoading}
          title={dedupGroups ? 'Show all findings' : 'Group duplicate findings'}
          aria-label="Toggle duplicate grouping"
          className={clsx(
            'text-2xs px-1.5 py-1 rounded transition-colors flex-shrink-0 disabled:opacity-50',
            dedupGroups
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3'
          )}
        >
          <Layers size={12} />
        </button>
        {report.findings.length > 0 && (
          <>
            <button
              onClick={handleExportCSV}
              className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
              aria-label="Export findings as CSV"
              title="Export as CSV"
            >
              <Download size={12} />
            </button>
            <button
              onClick={handleExportHTML}
              className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
              aria-label="Export findings as HTML"
              title="Export as HTML"
            >
              <FileText size={12} />
            </button>
          </>
        )}
      </div>
      {dedupGroups && (
        <div className="px-3 py-1 flex items-center justify-between text-2xs text-brand-400 bg-brand-500/5 border-b border-border-subtle">
          <span>Grouped: {findings.length} unique findings ({report.findings.length - findings.length} duplicates folded)</span>
          <button
            onClick={() => setDedupGroups(null)}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            Show all
          </button>
        </div>
      )}
      {suppressedCount > 0 && (
        <div className="px-3 py-1 flex items-center justify-between text-2xs text-text-tertiary border-b border-border-subtle">
          <span>{suppressedCount} finding{suppressedCount !== 1 ? 's' : ''} suppressed</span>
          <button
            onClick={() => useAnalysisStore.getState().clearSuppressed()}
            className="text-brand-400 hover:text-brand-300 px-1.5 py-0.5 rounded hover:bg-brand-500/10 transition-colors"
          >
            Restore all
          </button>
        </div>
      )}
      {diff ? (
        <AnalysisDiffView diff={diff} blockLookup={blockLookup} onBack={() => setDiff(null)} onFixWithAI={handleFixWithAI} />
      ) : (
        <>
          {report.findings.length > 0 && (
            <div className="px-3 py-1.5 border-b border-border-subtle">
              <div className="relative">
                <Search size={12} className="absolute left-2 top-1/2 -translate-y-1/2 text-text-disabled" />
                <input
                  type="text"
                  value={findingSearch}
                  onChange={e => setFindingSearch(e.target.value)}
                  placeholder="Search findings..."
                  className="w-full bg-surface-2 border border-border-subtle rounded-md pl-7 pr-2 py-1 text-2xs text-text-primary placeholder:text-text-disabled focus:outline-none focus:border-brand-500/50"
                />
              </div>
            </div>
          )}
          {findings.length === 0 ? (
            report.findings.length === 0 ? (
              <EmptyState
                title="No findings"
                description="The analysis didn't detect any issues. Your flow looks good!"
              />
            ) : (
              <div className="flex items-center justify-center h-full text-sm text-text-tertiary">
                No matching findings
              </div>
            )
          ) : (
            <FindingsList findings={findings} blockLookup={blockLookup} onFixWithAI={handleFixWithAI} sortMode={sortMode} />
          )}
        </>
      )}
    </div>
  )
}
