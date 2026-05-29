import {useCallback, useEffect} from 'react'
import {analysisApi} from '@/api'
import {subscribeToEvents} from '@/api/client'
import {useAnalysisStore} from '@/stores/analysisStore'
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
  const appendMessage = useChatStore(s => s.appendMessage)
  const createThread = useChatStore(s => s.createThread)
  const updateThread = useChatStore(s => s.updateThread)
  const switchThread = useChatStore(s => s.switchThread)
  const toggleSeverityFilter = useAnalysisStore(s => s.toggleSeverityFilter)
  const setSeverityFilter = useAnalysisStore(s => s.setSeverityFilter)
  const setInspectorTab = useUIStore(s => s.setInspectorTab)

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

  const findings = report!.findings.filter(f => severityFilter.has(f.severity))

  return (
    <div className="flex flex-col h-full">
      <FindingsSummary stats={report!.stats} durationMs={report!.durationMs} />
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
                'text-[9px] font-bold px-2 py-0.5 rounded-full border transition-all',
                severityFilter.has(s)
                  ? `${bg} ${color} border-transparent`
                  : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary'
              )}
            >
              {label}
            </button>
          ))}
          {severityFilter.size < 3 && (
            <button
              onClick={() => setSeverityFilter(new Set(['error', 'warning', 'info']))}
              className="text-[9px] text-text-tertiary hover:text-text-secondary transition-colors ml-1"
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
      </div>
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
