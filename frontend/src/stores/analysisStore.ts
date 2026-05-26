import {create} from 'zustand'
import type {AnalysisReport, Severity, Finding, VariableHistory} from '@/types/domain'

interface AnalysisState {
  reports: Map<string, AnalysisReport>
  isAnalyzing: boolean
  progress: {current: number; total: number; ruleName: string}
  severityFilter: Set<Severity>
  variableLineage: VariableHistory | null

  setReport: (flowId: string, report: AnalysisReport) => void
  setAnalyzing: (b: boolean) => void
  setProgress: (p: {current: number; total: number; ruleName: string}) => void
  toggleSeverityFilter: (s: Severity) => void
  setSeverityFilter: (s: Set<Severity>) => void
  findingsForBlock: (flowId: string, blockId: string) => Finding[]
  setVariableLineage: (h: VariableHistory | null) => void
}

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  isAnalyzing: false,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: new Set(['error', 'warning', 'info']),
  variableLineage: null,

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

  findingsForBlock: (flowId, blockId) => {
    const report = get().reports.get(flowId)
    if (!report) return []
    return report.findings.filter(f => f.blockId === blockId)
  },

  setVariableLineage: (h) => set({variableLineage: h}),
}))
