import {useCallback, useEffect, useMemo, useState, lazy, Suspense} from 'react'
import {Search} from 'lucide-react'
import {analysisApi, flowApi} from '@/api'
import {useAnalysisStore, findingKey, FINDING_CATEGORIES} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {useAuthStore} from '@/stores/authStore'
import {EmptyState, useToast} from '@/components/shared'
import {stageFindingFix} from '@/lib/fixWithAI'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {refreshAfterBlockEdit} from '@/lib/blockEdit'
import {SEARCH_DEBOUNCE_MS} from '@/lib/constants'
import {writeClipboard} from '@/lib/clipboard'
import FindingsSummary from './FindingsSummary'
import FindingsToolbar from './FindingsToolbar'
import AnalysisRunner from './AnalysisRunner'
import AnalysisProgressStrip from './AnalysisProgressStrip'
import AnalysisDiffView from './AnalysisDiffView'
import SuppressionsPanel from './SuppressionsPanel'
import {saveFindingsCSV, exportFindingsHTML, exportFindingsSARIF, exportFindingsJUnit} from '@/lib/findingsExport'
import {buildBlockLookup, type BlockLookup} from '@/lib/tree'

const FindingsList = lazy(() => import('./FindingsList'))
import {useTranslation} from 'react-i18next'
import {TRIAGE_STATUSES, isKnown, type AnalysisDiff, type Finding, type AnalysisReport} from '@/types'

export default function FindingsTab() {
  const {t} = useTranslation('findings')
  const doc = useFlowStore(s => s.document)
  const report = useAnalysisStore(s => (doc ? s.reports.get(doc.id) : undefined))
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const progress = useAnalysisStore(s => s.progress)
  const setReport = useAnalysisStore(s => s.setReport)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const setProgress = useAnalysisStore(s => s.setProgress)
  const findingSearch = useAnalysisStore(s => s.findingSearch)
  const setFindingSearch = useAnalysisStore(s => s.setFindingSearch)
  const [searchInput, setSearchInput] = useState(findingSearch)
  const [prevFindingSearch, setPrevFindingSearch] = useState(findingSearch)
  if (prevFindingSearch !== findingSearch) {
    setPrevFindingSearch(findingSearch)
    setSearchInput(findingSearch)
  }
  useEffect(() => {
    if (searchInput === findingSearch) return
    const t = setTimeout(() => setFindingSearch(searchInput), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(t)
  }, [searchInput, findingSearch, setFindingSearch])
  const severityFilter = useAnalysisStore(s => s.severityFilter)
  const hiddenCategories = useAnalysisStore(s => s.hiddenCategories)
  const statusFilter = useAnalysisStore(s => s.statusFilter)
  const assignedToMeOnly = useAnalysisStore(s => s.assignedToMeOnly)
  const triageMap = useAnalysisStore(s => s.triageMap)
  const currentUserId = useAuthStore(s => s.user?.id ?? '')
  const suppressedKeys = useAnalysisStore(s => s.suppressedKeys)
  const loadSuppressions = useAnalysisStore(s => s.loadSuppressions)
  const loadBaseline = useAnalysisStore(s => s.loadBaseline)
  const toast = useToast()
  const [diff, setDiff] = useState<AnalysisDiff | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)
  const [dedupGroups, setDedupGroups] = useState<Map<string, number> | null>(null)
  const [dedupLoading, setDedupLoading] = useState(false)
  const [sortMode, setSortMode] = useState<'default' | 'severity' | 'count'>('severity')

  const reportIdentity = `${doc?.id ?? ''}|${report?.generatedAt ?? ''}`
  const [prevReportIdentity, setPrevReportIdentity] = useState(reportIdentity)
  if (prevReportIdentity !== reportIdentity) {
    setPrevReportIdentity(reportIdentity)
    setDiff(null)
    setDedupGroups(null)
  }

  useEffect(() => {
    if (doc?.id) {
      void loadSuppressions(doc.id)
      void loadBaseline(doc.id)
    }
  }, [doc?.id, loadSuppressions, loadBaseline])

  const blockLookup = useMemo<BlockLookup>(() => (doc ? buildBlockLookup(doc) : new Map()), [doc])

  const baseFindings = useMemo(() => {
    if (!report) return []
    const q = findingSearch.toLowerCase()
    return report.findings.filter(f => {
      if (dedupGroups && !dedupGroups.has(f.id)) return false
      if (f.category && hiddenCategories.has(f.category)) return false
      if (suppressedKeys.has(findingKey(f))) return false
      const triage = triageMap.get(findingKey(f))
      const status = isKnown(TRIAGE_STATUSES, triage?.status) ? triage.status : 'open'
      if (statusFilter.size > 0 && !statusFilter.has(status)) return false
      if (assignedToMeOnly && (!triage?.assigneeId || triage.assigneeId !== currentUserId)) return false
      if (
        q &&
        !f.title.toLowerCase().includes(q) &&
        !f.description.toLowerCase().includes(q) &&
        !f.ruleId.toLowerCase().includes(q)
      )
        return false
      return true
    })
  }, [
    report,
    hiddenCategories,
    statusFilter,
    assignedToMeOnly,
    triageMap,
    currentUserId,
    findingSearch,
    suppressedKeys,
    dedupGroups,
  ])

  const findings = useMemo(
    () => baseFindings.filter(f => severityFilter.has(f.severity)),
    [baseFindings, severityFilter],
  )

  const categoryChips = useMemo(() => {
    const known = new Set<string>(FINDING_CATEGORIES)
    const extra = new Set<string>()
    for (const f of report?.findings ?? []) if (f.category && !known.has(f.category)) extra.add(f.category)
    for (const c of hiddenCategories) if (!known.has(c)) extra.add(c)
    return [...FINDING_CATEGORIES, ...[...extra].sort()]
  }, [report, hiddenCategories])

  const severityCounts = useMemo(() => {
    const counts: Record<Finding['severity'], number> = {error: 0, warning: 0, info: 0}
    for (const f of baseFindings) counts[f.severity]++
    return counts
  }, [baseFindings])

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
    if (useAnalysisStore.getState().isAnalyzing) return
    const gen = beginAnalyzing()
    setProgress({current: 0, total: 0, ruleName: ''})

    try {
      const r = await analysisApi.analyzeFlow()
      if (r) {
        setReport(doc.id, r as AnalysisReport)
      }
    } catch (err) {
      toast.error(t('toasts.analysisFailed'), {description: getErrorMessage(err)})
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
    }
  }, [doc, t, toast, setReport, setAnalyzing, beginAnalyzing, setProgress])

  const handleFixWithAI = useCallback(
    (finding: Finding) => {
      if (!doc) return
      stageFindingFix(finding, doc.id)
    },
    [doc],
  )

  const handleShowDiff = useCallback(async () => {
    setDiffLoading(true)
    try {
      const d = await analysisApi.getDiff()
      setDiff(d)
    } catch (err) {
      toast.error(t('toasts.diffFailed'), {description: getErrorMessage(err)})
    } finally {
      setDiffLoading(false)
    }
  }, [setDiffLoading, setDiff, t, toast])

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
      toast.error(t('toasts.deduplicateFailed'), {description: getErrorMessage(err)})
    } finally {
      setDedupLoading(false)
    }
  }, [dedupGroups, t, toast, setDedupGroups, setDedupLoading])

  const handleExportCSV = useCallback(async () => {
    if (!report) return
    try {
      await saveFindingsCSV(findings, doc?.id ?? 'report')
    } catch (err) {
      toast.error(t('toasts.csvExportFailed'), {description: getErrorMessage(err)})
    }
  }, [report, findings, doc, t, toast])

  const handleExportHTML = useCallback(async () => {
    if (!doc) return
    try {
      await exportFindingsHTML(doc.id)
    } catch (err) {
      toast.error(t('toasts.htmlExportFailed'), {description: getErrorMessage(err)})
    }
  }, [doc, t, toast])

  const handleExportJUnit = useCallback(async () => {
    if (!doc) return
    try {
      await exportFindingsJUnit(doc.id)
    } catch (err) {
      toast.error(t('toasts.junitExportFailed'), {description: getErrorMessage(err)})
    }
  }, [doc, t, toast])

  const handleExportSARIF = useCallback(async () => {
    if (!doc) return
    try {
      await exportFindingsSARIF(doc.id)
    } catch (err) {
      toast.error(t('toasts.sarifExportFailed'), {description: getErrorMessage(err)})
    }
  }, [doc, t, toast])

  const [snapshots, setSnapshots] = useState<{id: string; label: string; createdAt: string; bytes: number}[]>([])
  const [snapshotsLoading, setSnapshotsLoading] = useState(false)
  const loadSnapshots = useCallback(async () => {
    if (!doc) return
    setSnapshotsLoading(true)
    try {
      const res = await flowApi.listSnapshots(doc.id)
      setSnapshots(res?.snapshots ?? [])
    } catch (err) {
      logger.warn('Failed to load snapshots', err)
      setSnapshots([])
    } finally {
      setSnapshotsLoading(false)
    }
  }, [doc])

  const handleRestoreSnapshot = useCallback(
    async (snapshotId: string) => {
      if (!doc) return
      try {
        const res = await flowApi.restoreSnapshot(doc.id, snapshotId)
        if (res?.document) refreshAfterBlockEdit(res.document)
        toast.success(t('toasts.snapshotRestored'))
        void loadSnapshots()
      } catch (err) {
        toast.error(t('toasts.restoreFailed'), {description: getErrorMessage(err)})
      }
    },
    [doc, toast, loadSnapshots, t],
  )

  const handleShare = useCallback(async () => {
    if (!doc) return
    try {
      const result = await flowApi.createShare(doc.id)
      const url = `${window.location.origin}/shared?token=${result.token}`
      await writeClipboard(url)
      const expires = result.expiresAt ? ` · expires ${new Date(result.expiresAt).toLocaleDateString()}` : ''
      toast.success(t('toasts.shareLinkCopied'), {description: `${url}${expires}`})
    } catch (err) {
      toast.error(t('toasts.shareLinkFailed'), {description: getErrorMessage(err)})
    }
  }, [doc, t, toast])

  if (!doc) {
    return (
      <div className="flex items-center justify-center h-full text-sm text-text-tertiary p-4 text-center">
        {t('tabEmpty.noFlow')}
      </div>
    )
  }

  if (!report) {
    return <AnalysisRunner onAnalyze={handleAnalyze} isAnalyzing={isAnalyzing} progress={progress} />
  }

  if (!report) return null

  return (
    <div className="flex flex-col h-full">
      {isAnalyzing && <AnalysisProgressStrip progress={progress} />}
      <FindingsSummary stats={report.stats} durationMs={report.durationMs} healthScore={report.metrics?.healthScore} />
      <FindingsToolbar
        onReanalyze={handleAnalyze}
        onShowDiff={handleShowDiff}
        diffLoading={diffLoading}
        onToggleDedup={handleToggleDedup}
        dedupActive={dedupGroups !== null}
        dedupLoading={dedupLoading}
        onExportCSV={handleExportCSV}
        onExportHTML={handleExportHTML}
        onExportSARIF={handleExportSARIF}
        onExportJUnit={handleExportJUnit}
        onShare={handleShare}
        snapshots={snapshots}
        snapshotsLoading={snapshotsLoading}
        onRestoreSnapshot={handleRestoreSnapshot}
        onOpenSnapshots={loadSnapshots}
        sortMode={sortMode}
        onSetSortMode={setSortMode}
        severityCounts={severityCounts}
        categories={categoryChips}
        hasFindings={report.findings.length > 0}
      />
      {dedupGroups && (
        <div className="px-3 py-1 flex items-center justify-between text-2xs text-brand-400 bg-brand-500/5 border-b border-border-subtle">
          <span>
            {t('dedup.groupedSummary', {
              unique: findings.length,
              duplicates: report.findings.length - findings.length,
            })}
          </span>
          <button
            onClick={() => setDedupGroups(null)}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            {t('dedup.showAll')}
          </button>
        </div>
      )}
      {suppressedCount > 0 && (
        <div className="px-3 py-1 flex items-center justify-between text-2xs text-text-tertiary border-b border-border-subtle">
          <span>{t('suppressed.summary', {count: suppressedCount})}</span>
          <button
            onClick={() => useAnalysisStore.getState().clearSuppressed()}
            className="text-brand-400 hover:text-brand-300 px-1.5 py-0.5 rounded hover:bg-brand-500/10 transition-colors"
          >
            {t('suppressed.restoreAll')}
          </button>
        </div>
      )}
      <SuppressionsPanel flowId={doc.id} reportVersion={report.generatedAt} />
      {diff ? (
        <AnalysisDiffView
          diff={diff}
          blockLookup={blockLookup}
          onBack={() => setDiff(null)}
          onFixWithAI={handleFixWithAI}
        />
      ) : (
        <>
          {report.findings.length > 0 && (
            <div className="px-3 py-1.5 border-b border-border-subtle">
              <div className="relative">
                <Search size={12} className="absolute left-2 top-1/2 -translate-y-1/2 text-text-disabled" />
                <input
                  type="text"
                  value={searchInput}
                  onChange={e => setSearchInput(e.target.value)}
                  placeholder={t('search.placeholder')}
                  className="w-full bg-surface-2 border border-border-subtle rounded-md pl-7 pr-2 py-1 text-2xs text-text-primary placeholder:text-text-disabled focus:outline-hidden focus:border-brand-500/50"
                />
              </div>
            </div>
          )}
          {findings.length === 0 ? (
            report.findings.length === 0 ? (
              <EmptyState title={t('empty.noneTitle')} description={t('empty.noneDescription')} />
            ) : (
              <div className="flex items-center justify-center h-full text-sm text-text-tertiary">
                {t('empty.noMatch')}
              </div>
            )
          ) : (
            <Suspense fallback={<div className="flex-1" />}>
              <FindingsList
                findings={findings}
                blockLookup={blockLookup}
                onFixWithAI={handleFixWithAI}
                sortMode={sortMode}
              />
            </Suspense>
          )}
        </>
      )}
    </div>
  )
}
