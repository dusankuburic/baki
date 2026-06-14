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
  isAnalyzing: boolean
  analyzingGen: number
  progress: {current: number; total: number; ruleName: string}
  severityFilter: Set<Severity>
  categoryFilter: Set<FindingCategory>
  variableLineage: VariableHistory | null
  suppressedFindings: SuppressedFinding[]
  findingSearch: string

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
}

const MAX_REPORTS = 20

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  isAnalyzing: false,
  analyzingGen: 0,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: new Set(['error', 'warning', 'info']),
  categoryFilter: new Set<FindingCategory>(['Security', 'Reliability', 'Performance', 'Style', 'Logic']),
  variableLineage: null,
  suppressedFindings: [],
  findingSearch: '',

  setReport: (flowId, report) => set(state => {
    const next = new Map(state.reports)
    next.set(flowId, report)
    // Evict oldest entries beyond the cap to prevent unbounded memory growth.
    while (next.size > MAX_REPORTS) {
      const oldest = next.keys().next().value
      if (oldest === undefined) break
      next.delete(oldest)
    }
    return {reports: next}
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
    const report = get().reports.get(flowId)
    if (!report) return []
    return report.findings.filter(f => f.blockId === blockId)
  },

  setVariableLineage: (h) => set({variableLineage: h}),

  setFindingSearch: (q) => set({findingSearch: q}),

  suppressFinding: (finding, reason) => set(state => ({
    suppressedFindings: [...state.suppressedFindings, {
      findingId: finding.id,
      ruleId: finding.ruleId,
      blockId: finding.blockId,
      reason,
      suppressedAt: new Date().toISOString(),
    }],
  })),

  suppressMany: (findings, reason) => set(state => {
    const already = new Set(state.suppressedFindings.map(s => s.findingId))
    const now = new Date().toISOString()
    const added = findings
      .filter(f => !already.has(f.id))
      .map(f => ({findingId: f.id, ruleId: f.ruleId, blockId: f.blockId, reason, suppressedAt: now}))
    if (added.length === 0) return state
    return {suppressedFindings: [...state.suppressedFindings, ...added]}
  }),

  unsuppressFinding: (findingId) => set(state => ({
    suppressedFindings: state.suppressedFindings.filter(s => s.findingId !== findingId),
  })),

  clearSuppressed: () => set({suppressedFindings: []}),

  isSuppressed: (findingId) => {
    return get().suppressedFindings.some(s => s.findingId === findingId)
  },
}))
