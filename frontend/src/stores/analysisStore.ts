import {create} from 'zustand'
import type {AnalysisReport, Severity, Finding, VariableHistory} from '@/types'
import {toggleSetMember} from '@/lib/collections'

export type FindingCategory = 'Security' | 'Reliability' | 'Performance' | 'Style' | 'Logic'

export interface SuppressedFinding {
  findingId: string
  ruleId: string
  blockId: string
  reason: string
  suppressedAt: string
}

interface AnalysisState {
  reports: Map<string, AnalysisReport>
  findingsByBlock: Map<string, Map<string, Finding[]>>
  isAnalyzing: boolean
  analyzingGen: number
  progress: {current: number; total: number; ruleName: string}
  severityFilter: Set<Severity>
  categoryFilter: Set<FindingCategory>
  variableLineage: VariableHistory | null
  suppressedFindings: SuppressedFinding[]
  suppressedIds: Set<string>
  findingSearch: string
  protectedFlowId: string | null

  setReport: (flowId: string, report: AnalysisReport) => void
  setAnalyzing: (b: boolean) => void
  beginAnalyzing: () => number
  setProgress: (p: {current: number; total: number; ruleName: string}) => void
  toggleSeverityFilter: (s: Severity) => void
  setSeverityFilter: (s: Set<Severity>) => void
  toggleCategoryFilter: (c: FindingCategory) => void
  setCategoryFilter: (c: Set<FindingCategory>) => void
  findingsForBlock: (flowId: string, blockId: string) => Finding[]
  setVariableLineage: (h: VariableHistory | null) => void
  setFindingSearch: (q: string) => void
  suppressFinding: (finding: Finding, reason: string) => void
  suppressMany: (findings: Finding[], reason: string) => void
  unsuppressFinding: (findingId: string) => void
  clearSuppressed: () => void
  isSuppressed: (findingId: string) => boolean
  setProtectedFlowId: (id: string | null) => void
  reset: () => void
}

const MAX_REPORTS = 20

const defaultSeverityFilter = (): Set<Severity> => new Set(['error', 'warning', 'info'])
const defaultCategoryFilter = (): Set<FindingCategory> =>
  new Set<FindingCategory>(['Security', 'Reliability', 'Performance', 'Style', 'Logic'])

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  findingsByBlock: new Map(),
  isAnalyzing: false,
  analyzingGen: 0,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: defaultSeverityFilter(),
  categoryFilter: defaultCategoryFilter(),
  variableLineage: null,
  suppressedFindings: [],
  suppressedIds: new Set(),
  findingSearch: '',
  protectedFlowId: null,

  setReport: (flowId, report) => set(state => {
    const next = new Map(state.reports)
    next.set(flowId, report)
    // Build per-block findings index for O(1) lookup in findingsForBlock.
    const nextIndex = new Map(state.findingsByBlock)
    const blockIndex = new Map<string, Finding[]>()
    for (const f of report.findings) {
      const arr = blockIndex.get(f.blockId)
      if (arr) arr.push(f)
      else blockIndex.set(f.blockId, [f])
    }
    nextIndex.set(flowId, blockIndex)
    // Evict oldest entries beyond the cap, but never evict the currently-open
    // flow's report — losing it causes the per-block findings UI to silently
    // go empty while the dashboard aggregate still shows it.
    const protectedId = state.protectedFlowId
    while (next.size > MAX_REPORTS) {
      let evicted = false
      for (const key of next.keys()) {
        if (key === protectedId) continue
        next.delete(key)
        nextIndex.delete(key)
        evicted = true
        break
      }
      if (!evicted) break
    }
    return {reports: next, findingsByBlock: nextIndex}
  }),

  setAnalyzing: (b) => set({isAnalyzing: b}),

  beginAnalyzing: () => {
    const gen = get().analyzingGen + 1
    set({isAnalyzing: true, analyzingGen: gen})
    return gen
  },

  setProgress: (p) => set({progress: p}),

  toggleSeverityFilter: (s) => set(state => ({
    severityFilter: toggleSetMember(state.severityFilter, s),
  })),

  setSeverityFilter: (s) => set({severityFilter: new Set(s)}),

  toggleCategoryFilter: (c) => set(state => ({
    categoryFilter: toggleSetMember(state.categoryFilter, c),
  })),

  setCategoryFilter: (c) => set({categoryFilter: new Set(c)}),

  findingsForBlock: (flowId, blockId) => {
    const flowIndex = get().findingsByBlock.get(flowId)
    if (!flowIndex) return []
    return flowIndex.get(blockId) ?? []
  },

  setVariableLineage: (h) => set({variableLineage: h}),

  setFindingSearch: (q) => set({findingSearch: q}),

  suppressFinding: (finding, reason) => set(state => {
    const ids = new Set(state.suppressedIds)
    ids.add(finding.id)
    return {
      suppressedFindings: [...state.suppressedFindings, {
        findingId: finding.id,
        ruleId: finding.ruleId,
        blockId: finding.blockId,
        reason,
        suppressedAt: new Date().toISOString(),
      }],
      suppressedIds: ids,
    }
  }),

  suppressMany: (findings, reason) => set(state => {
    const added = findings
      .filter(f => !state.suppressedIds.has(f.id))
      .map(f => ({findingId: f.id, ruleId: f.ruleId, blockId: f.blockId, reason, suppressedAt: new Date().toISOString()}))
    if (added.length === 0) return state
    const ids = new Set(state.suppressedIds)
    for (const a of added) ids.add(a.findingId)
    return {suppressedFindings: [...state.suppressedFindings, ...added], suppressedIds: ids}
  }),

  unsuppressFinding: (findingId) => set(state => {
    const ids = new Set(state.suppressedIds)
    ids.delete(findingId)
    return {
      suppressedFindings: state.suppressedFindings.filter(s => s.findingId !== findingId),
      suppressedIds: ids,
    }
  }),

  clearSuppressed: () => set({suppressedFindings: [], suppressedIds: new Set()}),

  isSuppressed: (findingId) => get().suppressedIds.has(findingId),

  setProtectedFlowId: (id) => set({protectedFlowId: id}),

  reset: () => set({
    reports: new Map(),
    findingsByBlock: new Map(),
    isAnalyzing: false,
    analyzingGen: 0,
    progress: {current: 0, total: 0, ruleName: ''},
    severityFilter: defaultSeverityFilter(),
    categoryFilter: defaultCategoryFilter(),
    variableLineage: null,
    suppressedFindings: [],
    suppressedIds: new Set(),
    findingSearch: '',
    protectedFlowId: null,
  }),
}))
