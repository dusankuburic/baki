import {create} from 'zustand'
import type {AnalysisReport, Severity, Finding, VariableHistory} from '@/types/domain'

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
  progress: {current: number; total: number; ruleName: string}
  severityFilter: Set<Severity>
  categoryFilter: Set<FindingCategory>
  variableLineage: VariableHistory | null
  suppressedFindings: SuppressedFinding[]
  findingSearch: string

  setReport: (flowId: string, report: AnalysisReport) => void
  setAnalyzing: (b: boolean) => void
  setProgress: (p: {current: number; total: number; ruleName: string}) => void
  toggleSeverityFilter: (s: Severity) => void
  setSeverityFilter: (s: Set<Severity>) => void
  toggleCategoryFilter: (c: FindingCategory) => void
  setCategoryFilter: (c: Set<FindingCategory>) => void
  findingsForBlock: (flowId: string, blockId: string) => Finding[]
  setVariableLineage: (h: VariableHistory | null) => void
  setFindingSearch: (q: string) => void
  suppressFinding: (finding: Finding, reason: string) => void
  unsuppressFinding: (findingId: string) => void
  isSuppressed: (findingId: string) => boolean
}

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  isAnalyzing: false,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: new Set(['error', 'warning', 'info']),
  categoryFilter: new Set<FindingCategory>(['Security', 'Reliability', 'Performance', 'Style', 'Logic']),
  variableLineage: null,
  suppressedFindings: [],
  findingSearch: '',

  setReport: (flowId, report) => set(state => {
    const next = new Map(state.reports)
    next.set(flowId, report)
    return {reports: next}
  }),

  setAnalyzing: (b) => set({isAnalyzing: b}),

  setProgress: (p) => set({progress: p}),

  toggleSeverityFilter: (s) => set(state => {
    const next = new Set(state.severityFilter)
    if (next.has(s)) { next.delete(s) } else { next.add(s) }
    return {severityFilter: next}
  }),

  setSeverityFilter: (s) => set({severityFilter: new Set(s)}),

  toggleCategoryFilter: (c) => set(state => {
    const next = new Set(state.categoryFilter)
    if (next.has(c)) { next.delete(c) } else { next.add(c) }
    return {categoryFilter: next}
  }),

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

  unsuppressFinding: (findingId) => set(state => ({
    suppressedFindings: state.suppressedFindings.filter(s => s.findingId !== findingId),
  })),

  isSuppressed: (findingId) => {
    return get().suppressedFindings.some(s => s.findingId === findingId)
  },
}))
