import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {AnalysisReport, Severity, Finding, VariableHistory} from '@/types'
import {toggleSetMember} from '@/lib/collections'
import {analysisApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'

export type FindingCategory = 'Security' | 'Reliability' | 'Performance' | 'Style' | 'Logic'

// findingKey is the stable identity for a finding: the backend's content-derived
// fingerprint (ruleId:blockId), falling back to computing it locally for older
// payloads. Suppression and triage are keyed by this — NOT by finding.id, which
// is a per-run index that shifts as findings come and go.
export function findingKey(f: Finding): string {
  return f.fingerprint || `${f.ruleId}:${f.blockId}`
}

export interface SuppressedFinding {
  key: string
  ruleId: string
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
  suppressedKeys: Set<string>
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
  unsuppressFinding: (finding: Finding) => void
  clearSuppressed: () => void
  isSuppressed: (finding: Finding) => boolean
  // loadSuppressions pulls persisted, team-shared triage state for a flow (cloud
  // mode) and replaces the local suppressed set with it.
  loadSuppressions: (flowId: string) => Promise<void>
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
  suppressedKeys: new Set(),
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

  // Suppression is keyed by the stable findingKey and, in cloud mode, persisted
  // as team-shared triage state (status="suppressed"). Updates are optimistic;
  // a failed persist reverts the local change. Desktop (Tauri) has no backend,
  // so it stays in-memory only.
  suppressFinding: (finding, reason) => {
    const key = findingKey(finding)
    set(state => {
      if (state.suppressedKeys.has(key)) return state
      const keys = new Set(state.suppressedKeys)
      keys.add(key)
      return {
        suppressedKeys: keys,
        suppressedFindings: [...state.suppressedFindings, {key, ruleId: finding.ruleId, reason, suppressedAt: new Date().toISOString()}],
      }
    })
    if (!isTauri()) {
      analysisApi.setFindingStatus({findingKey: key, ruleId: finding.ruleId, status: 'suppressed', justification: reason})
        .catch(err => {
          logger.warn('Failed to persist suppression', err)
          set(state => {
            const keys = new Set(state.suppressedKeys)
            keys.delete(key)
            return {suppressedKeys: keys, suppressedFindings: state.suppressedFindings.filter(s => s.key !== key)}
          })
        })
    }
  },

  suppressMany: (findings, reason) => {
    const toAdd = findings.filter(f => !get().suppressedKeys.has(findingKey(f)))
    if (toAdd.length === 0) return
    set(state => {
      const keys = new Set(state.suppressedKeys)
      const added: SuppressedFinding[] = []
      for (const f of toAdd) {
        const key = findingKey(f)
        keys.add(key)
        added.push({key, ruleId: f.ruleId, reason, suppressedAt: new Date().toISOString()})
      }
      return {suppressedKeys: keys, suppressedFindings: [...state.suppressedFindings, ...added]}
    })
    if (!isTauri()) {
      for (const f of toAdd) {
        analysisApi.setFindingStatus({findingKey: findingKey(f), ruleId: f.ruleId, status: 'suppressed', justification: reason})
          .catch(err => logger.warn('Failed to persist suppression', err))
      }
    }
  },

  unsuppressFinding: (finding) => {
    const key = findingKey(finding)
    set(state => {
      const keys = new Set(state.suppressedKeys)
      keys.delete(key)
      return {suppressedKeys: keys, suppressedFindings: state.suppressedFindings.filter(s => s.key !== key)}
    })
    if (!isTauri()) {
      analysisApi.clearFindingStatus(key).catch(err => logger.warn('Failed to clear suppression', err))
    }
  },

  clearSuppressed: () => set({suppressedFindings: [], suppressedKeys: new Set()}),

  isSuppressed: (finding) => get().suppressedKeys.has(findingKey(finding)),

  loadSuppressions: async (flowId) => {
    if (isTauri() || !flowId) return
    try {
      const statuses = await analysisApi.listFindingStatuses(flowId)
      const suppressed = (statuses || []).filter(s => s.status === 'suppressed')
      set({
        suppressedKeys: new Set(suppressed.map(s => s.findingKey)),
        suppressedFindings: suppressed.map(s => ({
          key: s.findingKey,
          ruleId: s.ruleId || '',
          reason: s.justification || '',
          suppressedAt: s.updatedAt,
        })),
      })
    } catch (err) {
      logger.warn('Failed to load suppressions', err)
    }
  },

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
    suppressedKeys: new Set(),
    findingSearch: '',
    protectedFlowId: null,
  }),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => useAnalysisStore.getState().reset())
