import {useState, useMemo, useRef, useEffect} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {CheckCircle2, CheckSquare, ChevronRight, EyeOff, Square, UserPlus, Wrench, X} from 'lucide-react'
import clsx from 'clsx'
import type {Finding, Severity} from '@/types'
import {severityTone} from '@/lib/severityTone'
import {refreshAfterBlockEdit} from '@/lib/blockEdit'
import {bulkFixMessage} from '@/lib/bulkFixMessage'
import {getErrorMessage} from '@/lib/getErrorMessage'
import type {BlockLookup} from '@/lib/tree'
import {flowApi} from '@/api'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {useAuthStore} from '@/stores/authStore'
import {useToast} from '@/components/shared'
import {useTranslation} from 'react-i18next'
import FindingCard from './FindingCard'
import {useSuppressReasonPrompt} from './hooks/useSuppressReasonPrompt'

interface Props {
  findings: Finding[]
  blockLookup: BlockLookup
  onFixWithAI?: (finding: Finding) => void
  sortMode?: 'default' | 'severity' | 'count'
}

interface RuleGroup {
  ruleId: string
  title: string
  severity: Severity
  description: string
  suggestion: string
  findings: Finding[]
}

type Row =
  | {kind: 'header'; group: RuleGroup; collapsed: boolean; groupEnd: boolean}
  | {kind: 'card'; group: RuleGroup; finding: Finding; groupEnd: boolean}

function groupByRule(findings: Finding[]): RuleGroup[] {
  const map = new Map<string, RuleGroup>()
  for (const f of findings) {
    const existing = map.get(f.ruleId)
    if (existing) {
      existing.findings.push(f)
    } else {
      map.set(f.ruleId, {
        ruleId: f.ruleId,
        title: f.title,
        severity: f.severity,
        description: f.description,
        suggestion: f.suggestion ?? '',
        findings: [f],
      })
    }
  }
  return Array.from(map.values())
}

const rowBar = (sev: Severity) => severityTone(sev).bar

export default function FindingsList({findings, blockLookup, onFixWithAI, sortMode = 'severity'}: Props) {
  const {t} = useTranslation('findings')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [batchFixing, setBatchFixing] = useState(false)
  const suppressMany = useAnalysisStore(s => s.suppressMany)
  const promptSuppressReason = useSuppressReasonPrompt()
  const triageFindingsBatch = useAnalysisStore(s => s.triageFindingsBatch)
  const currentUserId = useAuthStore(s => s.user?.id ?? '')
  const selectedFindingIds = useAnalysisStore(s => s.selectedFindingIds)
  const toggleFindingSelection = useAnalysisStore(s => s.toggleFindingSelection)
  const selectAllFindings = useAnalysisStore(s => s.selectAllFindings)
  const clearFindingSelection = useAnalysisStore(s => s.clearFindingSelection)
  const readOnly = useFlowStore(s => s.readOnly)
  const docId = useFlowStore(s => s.document?.id)
  const reportFindings = useAnalysisStore(s => (docId ? s.reports.get(docId)?.findings : undefined))
  const selectedFindings = useMemo(
    () => (reportFindings ?? []).filter(f => selectedFindingIds.has(f.id)),
    [reportFindings, selectedFindingIds],
  )
  const hasFixableSelection = useMemo(
    () => (reportFindings ?? []).some(f => selectedFindingIds.has(f.id) && f.autoFix),
    [reportFindings, selectedFindingIds],
  )
  const toast = useToast()

  const handleBulkFix = async () => {
    const docId = useFlowStore.getState().document?.id
    if (!docId) return
    const report = useAnalysisStore.getState().reports.get(docId)
    const allFindings = report?.findings ?? []
    const selected = allFindings.filter(f => selectedFindingIds.has(f.id) && f.autoFix)
    if (selected.length === 0) return
    const rules = Array.from(new Set(selected.map(f => f.ruleId)))
    setBatchFixing(true)
    try {
      const {document: updated, applied, partial} = await flowApi.applyFixBatch(docId, rules)
      refreshAfterBlockEdit(updated)
      const msg = bulkFixMessage(applied, rules.length, partial === true)
      toast[msg.variant](msg.title, {description: msg.description})
      clearFindingSelection()
    } catch (err) {
      toast.error(t('toasts.bulkFixFailed'), {description: getErrorMessage(err)})
    } finally {
      setBatchFixing(false)
    }
  }

  const groups = useMemo(() => {
    const g = groupByRule(findings)
    if (sortMode === 'severity') {
      const order: Record<Severity, number> = {error: 0, warning: 1, info: 2}
      return [...g].sort((a, b) => order[a.severity] - order[b.severity])
    }
    if (sortMode === 'count') {
      return [...g].sort((a, b) => b.findings.length - a.findings.length)
    }
    return g
  }, [findings, sortMode])

  const rows = useMemo<Row[]>(() => {
    const out: Row[] = []
    for (const group of groups) {
      const isCollapsed = collapsed.has(group.ruleId)
      out.push({kind: 'header', group, collapsed: isCollapsed, groupEnd: isCollapsed})
      if (!isCollapsed) {
        group.findings.forEach((finding, i) => {
          out.push({kind: 'card', group, finding, groupEnd: i === group.findings.length - 1})
        })
      }
    }
    return out
  }, [groups, collapsed])

  const toggle = (ruleId: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(ruleId)) next.delete(ruleId)
      else next.add(ruleId)
      return next
    })
  }

  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const focusedFindingKey = useAnalysisStore(s => s.focusedFindingKey)
  const setFocusedFinding = useAnalysisStore(s => s.setFocusedFinding)
  useEffect(() => {
    if (!focusedFindingKey) return
    const idx = rows.findIndex(r => r.kind === 'card' && findingKey(r.finding) === focusedFindingKey)
    if (idx >= 0) {
      virtuosoRef.current?.scrollToIndex({index: idx, align: 'center'})
      const t = setTimeout(() => setFocusedFinding(null), 2500)
      return () => clearTimeout(t)
    }
    const g = groups.find(group => group.findings.some(f => findingKey(f) === focusedFindingKey))
    if (g && collapsed.has(g.ruleId)) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setCollapsed(prev => {
        const n = new Set(prev)
        n.delete(g.ruleId)
        return n
      })
      return
    }
    setFocusedFinding(null)
  }, [focusedFindingKey, rows, groups, collapsed, setFocusedFinding])

  return (
    <div className="flex-1 min-h-0 relative">
      <Virtuoso
        ref={virtuosoRef}
        style={{height: '100%'}}
        data={rows}
        computeItemKey={(_index, row) => (row.kind === 'header' ? `h:${row.group.ruleId}` : row.finding.id)}
        itemContent={(_index, row) => {
          const frame = clsx('border-l-2', rowBar(row.group.severity), row.groupEnd && 'border-b border-border-subtle')
          if (row.kind === 'header') {
            const {group, collapsed: isCollapsed} = row
            return (
              <div className={frame}>
                <div className="w-full px-3 py-2.5 flex items-start gap-2 hover:bg-surface-2 transition-colors group/header">
                  <button
                    onClick={() => toggle(group.ruleId)}
                    aria-expanded={!isCollapsed}
                    className="flex-1 flex items-start gap-2 text-left min-w-0"
                  >
                    <ChevronRight
                      size={14}
                      className={clsx(
                        'mt-0.5 shrink-0 text-text-tertiary transition-transform duration-fast',
                        !isCollapsed && 'rotate-90',
                      )}
                    />
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium text-text-primary truncate">{group.title}</span>
                        <span className="text-2xs text-text-tertiary shrink-0 tabular-nums">
                          {group.findings.length}×
                        </span>
                      </div>
                      {!isCollapsed && (
                        <p className="text-2xs text-text-secondary mt-1 leading-relaxed">{group.description}</p>
                      )}
                    </div>
                  </button>
                  <button
                    onClick={() => selectAllFindings(group.findings.map(f => f.id))}
                    aria-label={t('group.selectAllTitle')}
                    title={t('group.selectAllAria')}
                    className="opacity-0 group-hover/header:opacity-100 focus-visible:opacity-100 shrink-0 mt-0.5 flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-all duration-fast"
                  >
                    {group.findings.every(f => selectedFindingIds.has(f.id)) && group.findings.length > 0 ? (
                      <CheckSquare size={12} />
                    ) : (
                      <Square size={12} />
                    )}
                  </button>
                  <button
                    onClick={async () => {
                      const reason = await promptSuppressReason(group.findings.length)
                      if (reason !== null) suppressMany(group.findings, reason)
                    }}
                    aria-label={t('group.suppressAllTitle', {count: group.findings.length})}
                    title={t('group.suppressAllAria')}
                    className="opacity-0 group-hover/header:opacity-100 focus-visible:opacity-100 shrink-0 mt-0.5 flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-all duration-fast"
                  >
                    <EyeOff size={12} />
                    <span className="hidden sm:inline">All</span>
                  </button>
                </div>
              </div>
            )
          }
          const isSelected = selectedFindingIds.has(row.finding.id)
          const isFocused = !!focusedFindingKey && findingKey(row.finding) === focusedFindingKey
          return (
            <div
              className={clsx(
                frame,
                'relative group transition-colors',
                isSelected && 'bg-brand-500/5',
                isFocused && 'ring-1 ring-inset ring-brand-400 bg-brand-500/10',
              )}
            >
              <button
                onClick={() => toggleFindingSelection(row.finding.id)}
                className="absolute left-2 top-2.5 z-10 text-text-disabled hover:text-text-secondary transition-colors"
                aria-label={isSelected ? t('selection.deselect') : t('selection.select')}
              >
                {isSelected ? (
                  <CheckSquare size={12} className="text-brand-400" />
                ) : (
                  <Square size={12} className="opacity-0 group-hover:opacity-100" />
                )}
              </button>
              <FindingCard finding={row.finding} blockLookup={blockLookup} onFixWithAI={onFixWithAI} />
            </div>
          )
        }}
      />
      {selectedFindingIds.size > 0 && (
        <div className="absolute bottom-0 left-0 right-0 bg-surface-2/95 backdrop-blur-sm border-t border-border-subtle px-4 py-2 flex items-center justify-between z-30 shadow-lg">
          <span className="text-2xs text-text-secondary font-medium">
            {t('selection.selectedCount', {count: selectedFindingIds.size})}
          </span>
          <div className="flex items-center gap-3">
            {hasFixableSelection && (
              <button
                onClick={handleBulkFix}
                disabled={batchFixing || readOnly}
                className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-surface-3 transition-colors disabled:opacity-50"
              >
                <Wrench size={11} />
                {batchFixing ? t('selection.fixing') : t('selection.fixAll')}
              </button>
            )}
            {currentUserId && (
              <button
                onClick={() => {
                  triageFindingsBatch(selectedFindings, {assigneeId: currentUserId, status: 'in_progress'})
                }}
                className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-surface-3 transition-colors"
                title={t('group.assignToMeTitle')}
              >
                <UserPlus size={11} />
                {t('selection.assignMe', 'Assign to me')}
              </button>
            )}
            <button
              onClick={() => triageFindingsBatch(selectedFindings, {status: 'resolved'})}
              className="flex items-center gap-1 text-2xs text-semantic-success px-2 py-1 rounded hover:bg-semantic-success/10 transition-colors"
              title={t('group.resolveTitle')}
            >
              <CheckCircle2 size={11} />
              {t('selection.resolve', 'Resolve')}
            </button>
            <button
              onClick={async () => {
                const reason = await promptSuppressReason(selectedFindingIds.size)
                if (reason === null) return
                const docId = useFlowStore.getState().document?.id
                const report = docId ? useAnalysisStore.getState().reports.get(docId) : undefined
                const allFindings = report?.findings ?? []
                const toSuppress = allFindings.filter(f => selectedFindingIds.has(f.id))
                suppressMany(toSuppress, reason)
                clearFindingSelection()
              }}
              className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 transition-colors"
            >
              <EyeOff size={11} />
              {t('selection.suppress')}
            </button>
            <button
              onClick={clearFindingSelection}
              className="text-text-tertiary hover:text-text-secondary p-1 rounded hover:bg-surface-3 transition-colors"
              aria-label={t('selection.clear')}
            >
              <X size={12} />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
