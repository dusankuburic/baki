import {useState, useMemo, useRef, useEffect} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {ChevronRight, EyeOff, CheckSquare, Square, X} from 'lucide-react'
import clsx from 'clsx'
import type {Finding, Severity} from '@/types'
import type {BlockLookup} from '@/lib/tree'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import FindingCard from './FindingCard'

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

// A flattened render row: each rule group contributes one header row, followed
// by one row per finding when expanded. Flattening lets the whole list render
// through a single virtualizer (react-virtuoso) so only the visible rows mount —
// large flows can produce tens of thousands of findings, and mounting them all
// froze the Findings tab on every open.
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

const sevColor: Record<Severity, string> = {
  error: 'border-l-red-500',
  warning: 'border-l-amber-500',
  info: 'border-l-blue-500',
}

export default function FindingsList({findings, blockLookup, onFixWithAI, sortMode = 'severity'}: Props) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const suppressMany = useAnalysisStore(s => s.suppressMany)
  const selectedFindingIds = useAnalysisStore(s => s.selectedFindingIds)
  const toggleFindingSelection = useAnalysisStore(s => s.toggleFindingSelection)
  const selectAllFindings = useAnalysisStore(s => s.selectAllFindings)
  const clearFindingSelection = useAnalysisStore(s => s.clearFindingSelection)

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

  // Flatten groups → header/card rows for the virtualizer. A collapsed group
  // contributes only its header; groupEnd marks the last visible row of a group
  // so it carries the bottom divider (the old per-group wrapper did this).
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

  // Chat "finding:" deep-links set focusedFindingKey. Reveal that finding:
  // expand its group if collapsed (which re-runs this effect once rows update),
  // scroll it to center, then clear the focus after the highlight has shown.
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
    // Not currently rendered — expand its (collapsed) group so it does, then
    // let the effect re-run against the updated rows.
    const g = groups.find(group => group.findings.some(f => findingKey(f) === focusedFindingKey))
    if (g && collapsed.has(g.ruleId)) {
      setCollapsed(prev => { const n = new Set(prev); n.delete(g.ruleId); return n })
      return
    }
    // Filtered out of this view entirely — nothing to reveal; drop the request.
    setFocusedFinding(null)
  }, [focusedFindingKey, rows, groups, collapsed, setFocusedFinding])

  return (
    <div className="flex-1 min-h-0 relative">
      <Virtuoso
        ref={virtuosoRef}
        style={{height: '100%'}}
        data={rows}
        computeItemKey={(_index, row) => row.kind === 'header' ? `h:${row.group.ruleId}` : row.finding.id}
        itemContent={(_index, row) => {
          const frame = clsx('border-l-2', sevColor[row.group.severity], row.groupEnd && 'border-b border-border-subtle')
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
                        !isCollapsed && 'rotate-90'
                      )}
                    />
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium text-text-primary truncate">{group.title}</span>
                        <span className="text-2xs text-text-tertiary shrink-0 tabular-nums">{group.findings.length}×</span>
                      </div>
                      {!isCollapsed && (
                        <p className="text-2xs text-text-secondary mt-1 leading-relaxed">{group.description}</p>
                      )}
                    </div>
                  </button>
                  <button
                    onClick={() => selectAllFindings(group.findings.map(f => f.id))}
                    aria-label="Select all findings in this group"
                    title="Select all in group"
                    className="opacity-0 group-hover/header:opacity-100 focus-visible:opacity-100 shrink-0 mt-0.5 flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-all duration-fast"
                  >
                    {group.findings.every(f => selectedFindingIds.has(f.id)) && group.findings.length > 0
                      ? <CheckSquare size={12} />
                      : <Square size={12} />}
                  </button>
                  <button
                    onClick={() => suppressMany(group.findings, `Suppressed all "${group.title}" findings`)}
                    aria-label={`Suppress all ${group.findings.length} findings of this rule`}
                    title="Suppress all findings in this group"
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
            <div className={clsx(
              frame, 'relative group transition-colors',
              isSelected && 'bg-brand-500/5',
              isFocused && 'ring-1 ring-inset ring-brand-400 bg-brand-500/10',
            )}>
              <button
                onClick={() => toggleFindingSelection(row.finding.id)}
                className="absolute left-2 top-2.5 z-10 text-text-disabled hover:text-text-secondary transition-colors"
                aria-label={isSelected ? 'Deselect finding' : 'Select finding'}
              >
                {isSelected
                  ? <CheckSquare size={12} className="text-brand-400" />
                  : <Square size={12} className="opacity-0 group-hover:opacity-100" />}
              </button>
              <FindingCard finding={row.finding} blockLookup={blockLookup} onFixWithAI={onFixWithAI} />
            </div>
          )
        }}
      />
      {selectedFindingIds.size > 0 && (
        <div className="absolute bottom-0 left-0 right-0 bg-surface-2/95 backdrop-blur border-t border-border-subtle px-4 py-2 flex items-center justify-between z-30 shadow-lg">
          <span className="text-2xs text-text-secondary font-medium">
            {selectedFindingIds.size} selected
          </span>
          <div className="flex items-center gap-3">
            <button
              onClick={() => {
                // Suppress ALL selected findings from the full report, not
                // just the filtered view — selected items hidden by a filter
                // change must not be silently skipped.
                const docId = useFlowStore.getState().document?.id
                const report = docId ? useAnalysisStore.getState().reports.get(docId) : undefined
                const allFindings = report?.findings ?? []
                const toSuppress = allFindings.filter(f => selectedFindingIds.has(f.id))
                suppressMany(toSuppress, 'Bulk suppressed via selection')
                clearFindingSelection()
              }}
              className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 transition-colors"
            >
              <EyeOff size={11} />
              Suppress
            </button>
            <button
              onClick={clearFindingSelection}
              className="text-text-tertiary hover:text-text-secondary p-1 rounded hover:bg-surface-3 transition-colors"
              aria-label="Clear selection"
            >
              <X size={12} />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
