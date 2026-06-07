import {useCallback, useEffect, useMemo} from 'react'
import {Download, FileText, Search} from 'lucide-react'
import {analysisApi} from '@/api'
import {subscribeToEvents} from '@/api/client'
import {useAnalysisStore, type FindingCategory} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {Spinner} from '@/components/shared'
import FindingsSummary from './FindingsSummary'
import FindingsList from './FindingsList'
import type {Finding, Severity} from '@/types/domain'
import clsx from 'clsx'

export default function FindingsTab() {
  const doc = useFlowStore(s => s.document)
  const report = useAnalysisStore(s => doc ? s.reports.get(doc.id) : undefined)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const progress = useAnalysisStore(s => s.progress)
  const setReport = useAnalysisStore(s => s.setReport)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
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
  const suppressedFindings = useAnalysisStore(s => s.suppressedFindings)

  const findings = useMemo(() => {
    if (!report) return []
    const isSuppressed = (id: string) => suppressedFindings.some(s => s.findingId === id)
    const q = findingSearch.toLowerCase()
    return report.findings.filter(f => {
      if (!severityFilter.has(f.severity)) return false
      if (f.category && !categoryFilter.has(f.category as FindingCategory)) return false
      if (isSuppressed(f.id)) return false
      if (q && !f.title.toLowerCase().includes(q) && !f.description.toLowerCase().includes(q) && !f.ruleId.toLowerCase().includes(q)) return false
      return true
    })
  }, [report, severityFilter, categoryFilter, findingSearch, suppressedFindings])

  const suppressedCount = useMemo(() => {
    if (!report) return 0
    const isSuppressed = (id: string) => suppressedFindings.some(s => s.findingId === id)
    return report.findings.filter(f => isSuppressed(f.id)).length
  }, [report, suppressedFindings])

  useEffect(() => {
    if (!isAnalyzing) return
    // Use a sync cancelled flag so cleanup is immediate — React does not await
    // async cleanup functions, so the Promise.then pattern leaks the listener on
    // fast unmount/remount (e.g. React StrictMode).
    let unsub: (() => void) | null = null
    let cancelled = false
    subscribeToEvents((ev) => {
      if (ev.name !== 'analysis:progress') return
      const data = ev.data
      setProgress({
        current: data.current ?? 0,
        total: data.total ?? 0,
        ruleName: data.ruleName ?? '',
      })
    }).then(fn => { if (!cancelled) unsub = fn; else fn() })
    return () => { cancelled = true; unsub?.() }
  }, [isAnalyzing, setProgress])

  const handleAnalyze = useCallback(async () => {
    if (!doc) return
    setAnalyzing(true)
    setProgress({current: 0, total: 0, ruleName: ''})

    try {
      const r = await analysisApi.analyzeFlow()
      if (r) {
        setReport(doc.id, r as any)
      }
    } catch (err) {
      console.error('analysis failed:', err)
    } finally {
      setAnalyzing(false)
    }
  }, [doc, setReport, setAnalyzing, setProgress])

  const handleFixWithAI = useCallback((finding: Finding) => {
    if (!doc) return
    const threadId = createThread(doc.id)
    updateThread(threadId, {
      title: `Fix: ${finding.title}`,
      contextBlockId: finding.blockId,
    })
    appendMessage(threadId, {
      id: crypto.randomUUID(),
      role: 'user',
      content: `Help me fix this issue: **${finding.title}**\n\n${finding.description}\n\n${finding.suggestion ?? ''}`,
      timestamp: new Date().toISOString(),
      contextBlockId: finding.blockId,
    })
    switchThread(threadId)
    setInspectorTab('ai')
  }, [appendMessage, createThread, updateThread, switchThread, setInspectorTab, doc])

  const handleExportCSV = useCallback(() => {
    if (!report) return
    const rows = [['ID', 'Severity', 'Category', 'Title', 'Description', 'Block ID', 'Subflow ID', 'Suggestion']]
    for (const f of report.findings) {
      rows.push([
        f.id,
        f.severity,
        f.category ?? '',
        `"${f.title.replace(/"/g, '""')}"`,
        `"${f.description.replace(/"/g, '""')}"`,
        f.blockId,
        f.subflowId,
        f.suggestion ? `"${f.suggestion.replace(/"/g, '""')}"` : '',
      ])
    }
    const csv = rows.map(r => r.join(',')).join('\n')
    const blob = new Blob([csv], {type: 'text/csv;charset=utf-8;'})
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `analysis-${doc?.id ?? 'report'}-${new Date().toISOString().slice(0, 10)}.csv`
    a.click()
    URL.revokeObjectURL(url)
  }, [report, doc])

  const handleExportHTML = useCallback(async () => {
    if (!doc) return
    try {
      const html = await analysisApi.exportHTML()
      const blob = new Blob([html as unknown as string], {type: 'text/html;charset=utf-8;'})
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `analysis-${doc.id}-${new Date().toISOString().slice(0, 10)}.html`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      console.error('HTML export failed:', err)
    }
  }, [doc])

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
          className="px-4 py-2 rounded-lg bg-accent text-white text-sm font-medium hover:bg-accent-light transition-colors"
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
        <span className="text-sm text-text-secondary">
          Analyzing... {pct}% ({progress.ruleName})
        </span>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full">
      <FindingsSummary stats={report!.stats} durationMs={report!.durationMs} healthScore={report!.metrics?.healthScore} />
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
            const catColors: Record<string, string> = {
              Security: 'text-red-400',
              Reliability: 'text-amber-400',
              Performance: 'text-orange-400',
              Style: 'text-purple-400',
              Logic: 'text-cyan-400',
            }
            const catBg: Record<string, string> = {
              Security: 'bg-red-500/10',
              Reliability: 'bg-amber-500/10',
              Performance: 'bg-orange-500/10',
              Style: 'bg-purple-500/10',
              Logic: 'bg-cyan-500/10',
            }
            return (
              <button
                key={cat}
                onClick={() => toggleCategoryFilter(cat)}
                className={clsx(
                  'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
                  categoryFilter.has(cat)
                    ? `${catBg[cat]} ${catColors[cat]} border-transparent`
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
        {report!.findings.length > 0 && (
          <>
            <button
              onClick={handleExportCSV}
              className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
              title="Export as CSV"
            >
              <Download size={12} />
            </button>
            <button
              onClick={handleExportHTML}
              className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
              title="Export as HTML"
            >
              <FileText size={12} />
            </button>
          </>
        )}
      </div>
      {suppressedCount > 0 && (
        <div className="px-3 py-1 text-2xs text-text-tertiary border-b border-border-subtle">
          {suppressedCount} finding{suppressedCount !== 1 ? 's' : ''} suppressed
        </div>
      )}
      {report!.findings.length > 0 && (
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
        <div className="flex items-center justify-center h-full text-sm text-text-tertiary">
          No findings
        </div>
      ) : (
        <FindingsList findings={findings} doc={doc} onFixWithAI={handleFixWithAI} />
      )}
    </div>
  )
}
