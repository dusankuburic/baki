import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {
  TRIAGE_STATUSES,
  isKnown,
  type AnalysisReport,
  type Severity,
  type Finding,
  type VariableHistory,
  type FindingStatus,
  type FlowBaseline,
  type TriageStatus,
} from '@/types'
import {toggleSetMember} from '@/lib/collections'
import {analysisApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'
import {notifyIfBackground} from '@/hooks/usePlatform'
import {useFlowStore} from './flowStore'

export const FINDING_CATEGORIES = [
  'Security',
  'Reliability',
  'Performance',
  'Style',
  'Logic',
  'Portability',
  'Syntax',
] as const
export type FindingCategory = (typeof FINDING_CATEGORIES)[number]

const LEGACY_VIEW_CATEGORIES: readonly string[] = ['Security', 'Reliability', 'Performance', 'Style', 'Logic']

export interface SavedFilterView {
  name: string
  severities: Severity[]
  hiddenCategories?: string[]
  categories: string[]
}

export function viewHiddenCategories(view: SavedFilterView): Set<string> {
  if (Array.isArray(view.hiddenCategories)) return new Set(view.hiddenCategories)
  const shown = new Set(Array.isArray(view.categories) ? view.categories : LEGACY_VIEW_CATEGORIES)
  return new Set(LEGACY_VIEW_CATEGORIES.filter(c => !shown.has(c)))
}

const SAVED_VIEWS_KEY = 'baki:savedFilterViews'

function loadSavedViews(): SavedFilterView[] {
  try {
    const raw = localStorage.getItem(SAVED_VIEWS_KEY)
    return raw ? (JSON.parse(raw) as SavedFilterView[]) : []
  } catch {
    return []
  }
}

function persistSavedViews(views: SavedFilterView[]) {
  try {
    localStorage.setItem(SAVED_VIEWS_KEY, JSON.stringify(views))
  } catch {
  }
}

export function findingKey(f: Finding): string {
  return f.fingerprint || `${f.ruleId}:${f.blockId}`
}

export interface SuppressedFinding {
  key: string
  ruleId: string
  reason: string
  suppressedAt: string
}

const EMPTY_BLOCK_INDEX: ReadonlyMap<string, Finding[]> = new Map()
const blockIndexCache = new WeakMap<AnalysisReport, Map<string, Finding[]>>()

export function findingsByBlockOf(report: AnalysisReport | undefined): ReadonlyMap<string, Finding[]> {
  if (!report) return EMPTY_BLOCK_INDEX
  let index = blockIndexCache.get(report)
  if (!index) {
    index = new Map<string, Finding[]>()
    for (const f of report.findings ?? []) {
      const arr = index.get(f.blockId)
      if (arr) arr.push(f)
      else index.set(f.blockId, [f])
    }
    blockIndexCache.set(report, index)
  }
  return index
}

interface AnalysisState {
  reports: Map<string, AnalysisReport>
  isAnalyzing: boolean
  analyzingGen: number
  progress: {current: number; total: number; ruleName: string}
  severityFilter: Set<Severity>
  hiddenCategories: Set<string>
  statusFilter: Set<TriageStatus>
  assignedToMeOnly: boolean
  variableLineage: VariableHistory | null
  suppressedFindings: SuppressedFinding[]
  suppressedKeys: Set<string>
  findingSearch: string
  protectedFlowId: string | null
  triageMap: Map<string, FindingStatus>
  baseline: FlowBaseline | null
  baselineNewCount: number | null
  savedViews: SavedFilterView[]
  selectedFindingIds: Set<string>
  focusedFindingKey: string | null

  setReport: (flowId: string, report: AnalysisReport) => void
  setFocusedFinding: (key: string | null) => void
  setAnalyzing: (b: boolean) => void
  beginAnalyzing: () => number
  setProgress: (p: {current: number; total: number; ruleName: string}) => void
  toggleSeverityFilter: (s: Severity) => void
  setSeverityFilter: (s: Set<Severity>) => void
  toggleCategory: (c: string) => void
  setHiddenCategories: (c: Iterable<string>) => void
  toggleStatusFilter: (st: TriageStatus) => void
  setStatusFilter: (sts: Set<TriageStatus>) => void
  toggleAssignedToMe: () => void
  setVariableLineage: (h: VariableHistory | null) => void
  setFindingSearch: (q: string) => void
  suppressFinding: (finding: Finding, reason: string) => void
  suppressMany: (findings: Finding[], reason: string) => void
  unsuppressFinding: (finding: Finding) => void
  clearSuppressed: () => void
  loadSuppressions: (flowId: string) => Promise<void>
  setFindingTriage: (finding: Finding, status: TriageStatus, justification?: string) => void
  triageFindingsBatch: (findings: Finding[], patch: {status?: TriageStatus; assigneeId?: string | null}) => void
  assignFinding: (finding: Finding, assigneeId: string | null) => void
  loadBaseline: (flowId: string) => Promise<void>
  handleSetBaseline: () => Promise<void>
  handleClearBaseline: () => Promise<void>
  saveCurrentView: (name: string, severities: Set<Severity>, hiddenCategories: Set<string>) => void
  deleteSavedView: (name: string) => void
  toggleFindingSelection: (id: string) => void
  selectAllFindings: (ids: string[]) => void
  clearFindingSelection: () => void
  setProtectedFlowId: (id: string | null) => void
  reset: () => void
}

const MAX_REPORTS = 20
export const MAX_RETAINED_FINDINGS = 100_000

const defaultSeverityFilter = (): Set<Severity> => new Set(['error', 'warning', 'info'])
const defaultStatusFilter = (): Set<TriageStatus> => new Set(['open', 'acknowledged', 'in_progress', 'resolved'])

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  isAnalyzing: false,
  analyzingGen: 0,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: defaultSeverityFilter(),
  hiddenCategories: new Set(),
  statusFilter: defaultStatusFilter(),
  assignedToMeOnly: false,
  variableLineage: null,
  suppressedFindings: [],
  suppressedKeys: new Set(),
  findingSearch: '',
  protectedFlowId: null,
  triageMap: new Map(),
  baseline: null,
  baselineNewCount: null,
  savedViews: loadSavedViews(),
  selectedFindingIds: new Set(),
  focusedFindingKey: null,

  setFocusedFinding: key => set({focusedFindingKey: key}),

  setReport: (flowId, report) => {
    set(state => {
      const next = new Map(state.reports)
      next.delete(flowId)
      next.set(flowId, report)
      const protectedId = state.protectedFlowId
      let retained = 0
      for (const r of next.values()) retained += r.findings?.length ?? 0
      for (const [key, r] of next) {
        if (next.size <= MAX_REPORTS && retained <= MAX_RETAINED_FINDINGS) break
        if (key === protectedId || key === flowId) continue
        next.delete(key)
        retained -= r.findings?.length ?? 0
      }
      return {reports: next}
    })
    const count = report.findings?.length ?? 0
    const noun = count === 1 ? 'finding' : 'findings'
    void notifyIfBackground({
      title: 'Analysis complete',
      body: count === 0 ? 'No findings — flow looks clean.' : `${count} ${noun} found.`,
    })
  },

  setAnalyzing: b => set({isAnalyzing: b}),

  beginAnalyzing: () => {
    const gen = get().analyzingGen + 1
    set({isAnalyzing: true, analyzingGen: gen})
    return gen
  },

  setProgress: p => set({progress: p}),

  toggleSeverityFilter: s =>
    set(state => ({
      severityFilter: toggleSetMember(state.severityFilter, s),
    })),

  setSeverityFilter: s => set({severityFilter: new Set(s)}),

  toggleCategory: c =>
    set(state => ({
      hiddenCategories: toggleSetMember(state.hiddenCategories, c),
    })),

  setHiddenCategories: c => set({hiddenCategories: new Set(c)}),

  toggleStatusFilter: (st: TriageStatus) =>
    set(state => ({
      statusFilter: toggleSetMember(state.statusFilter, st),
    })),

  setStatusFilter: (sts: Set<TriageStatus>) => set({statusFilter: new Set(sts)}),

  toggleAssignedToMe: () => set(state => ({assignedToMeOnly: !state.assignedToMeOnly})),

  setVariableLineage: h => set({variableLineage: h}),

  setFindingSearch: q => set({findingSearch: q}),

  suppressFinding: (finding, reason) => {
    const key = findingKey(finding)
    set(state => {
      if (state.suppressedKeys.has(key)) return state
      const keys = new Set(state.suppressedKeys)
      keys.add(key)
      return {
        suppressedKeys: keys,
        suppressedFindings: [
          ...state.suppressedFindings,
          {key, ruleId: finding.ruleId, reason, suppressedAt: new Date().toISOString()},
        ],
      }
    })
    if (!isTauri()) {
      analysisApi
        .setFindingStatus({findingKey: key, ruleId: finding.ruleId, status: 'suppressed', justification: reason})
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
      const addedKeys = new Set(toAdd.map(findingKey))
      analysisApi
        .setFindingStatusBatch(
          toAdd.map(f => ({
            findingKey: findingKey(f),
            ruleId: f.ruleId,
            status: 'suppressed' as const,
            justification: reason,
          })),
        )
        .catch(err => {
          logger.warn('Failed to persist bulk suppression', err)
          set(state => {
            const keys = new Set(state.suppressedKeys)
            for (const k of addedKeys) keys.delete(k)
            return {
              suppressedKeys: keys,
              suppressedFindings: state.suppressedFindings.filter(s => !addedKeys.has(s.key)),
            }
          })
        })
    }
  },

  unsuppressFinding: finding => {
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

  loadSuppressions: async flowId => {
    if (isTauri() || !flowId) return
    try {
      const statuses = await analysisApi.listFindingStatuses(flowId)
      if (useFlowStore.getState().document?.id !== flowId) return
      const suppressed = (statuses || []).filter(s => s.status === 'suppressed')
      const triageMap = new Map<string, FindingStatus>()
      for (const s of statuses || []) {
        triageMap.set(s.findingKey, s)
      }
      set({
        suppressedKeys: new Set(suppressed.map(s => s.findingKey)),
        suppressedFindings: suppressed.map(s => ({
          key: s.findingKey,
          ruleId: s.ruleId || '',
          reason: s.justification || '',
          suppressedAt: s.updatedAt,
        })),
        triageMap,
      })
    } catch (err) {
      logger.warn('Failed to load suppressions', err)
    }
  },

  setFindingTriage: (finding, status, justification) => {
    const key = findingKey(finding)
    const preservedAssignee = get().triageMap.get(key)?.assigneeId
    set(state => {
      const triageMap = new Map(state.triageMap)
      if (status === 'open') {
        triageMap.delete(key)
      } else {
        triageMap.set(key, {
          flowId: useFlowStore.getState().document?.id ?? '',
          findingKey: key,
          ruleId: finding.ruleId,
          status,
          justification,
          assigneeId: preservedAssignee,
          updatedAt: new Date().toISOString(),
        })
      }
      const isSuppressed = status === 'suppressed'
      if (isSuppressed && !state.suppressedKeys.has(key)) {
        const keys = new Set(state.suppressedKeys)
        keys.add(key)
        return {
          triageMap,
          suppressedKeys: keys,
          suppressedFindings: [
            ...state.suppressedFindings,
            {key, ruleId: finding.ruleId, reason: justification ?? '', suppressedAt: new Date().toISOString()},
          ],
        }
      }
      if (!isSuppressed && state.suppressedKeys.has(key)) {
        const keys = new Set(state.suppressedKeys)
        keys.delete(key)
        return {
          triageMap,
          suppressedKeys: keys,
          suppressedFindings: state.suppressedFindings.filter(s => s.key !== key),
        }
      }
      return {triageMap}
    })
    if (!isTauri()) {
      analysisApi
        .setFindingStatus({
          findingKey: key,
          ruleId: finding.ruleId,
          status,
          justification,
          assigneeId: preservedAssignee,
        })
        .catch(err => logger.warn('Failed to persist triage status', err))
    }
  },

  triageFindingsBatch: (findings, patch) => {
    if (findings.length === 0) return
    const flowId = useFlowStore.getState().document?.id ?? ''
    const prev = findings.map(f => get().triageMap.get(findingKey(f)))
    set(state => {
      const triageMap = new Map(state.triageMap)
      for (const f of findings) {
        const key = findingKey(f)
        const existing = triageMap.get(key)
        const status = patch.status ?? existing?.status ?? 'in_progress'
        if (!isKnown(TRIAGE_STATUSES, status)) continue
        const assigneeId = patch.assigneeId === undefined ? existing?.assigneeId : patch.assigneeId || undefined
        if (status === 'open' && !assigneeId) {
          triageMap.delete(key)
          continue
        }
        triageMap.set(key, {
          flowId,
          findingKey: key,
          ruleId: f.ruleId,
          status,
          assigneeId,
          updatedAt: new Date().toISOString(),
        })
      }
      return {triageMap}
    })
    if (!isTauri()) {
      const items = findings.flatMap((f, i) => {
        const existing = prev[i]
        const status = patch.status ?? existing?.status ?? 'in_progress'
        if (!isKnown(TRIAGE_STATUSES, status)) return []
        const assigneeId = patch.assigneeId === undefined ? existing?.assigneeId : patch.assigneeId || undefined
        return [{findingKey: findingKey(f), ruleId: f.ruleId, status, assigneeId}]
      })
      if (items.length === 0) return
      analysisApi.setFindingStatusBatch(items).catch(err => {
        logger.warn('Failed to persist bulk triage', err)
        set(state => {
          const triageMap = new Map(state.triageMap)
          findings.forEach((f, i) => {
            const key = findingKey(f)
            const before = prev[i]
            if (before) triageMap.set(key, before)
            else triageMap.delete(key)
          })
          return {triageMap}
        })
      })
    }
  },

  assignFinding: (finding, assigneeId) => {
    const key = findingKey(finding)
    const status = get().triageMap.get(key)?.status ?? 'open'
    if (!isKnown(TRIAGE_STATUSES, status)) return
    set(state => {
      const triageMap = new Map(state.triageMap)
      if (status === 'open' && !assigneeId) {
        if (!triageMap.has(key)) return state
        triageMap.delete(key)
        return {triageMap}
      }
      triageMap.set(key, {
        flowId: useFlowStore.getState().document?.id ?? '',
        findingKey: key,
        ruleId: finding.ruleId,
        status,
        assigneeId: assigneeId ?? undefined,
        updatedAt: new Date().toISOString(),
      })
      return {triageMap}
    })
    if (!isTauri()) {
      analysisApi
        .setFindingStatus({findingKey: key, ruleId: finding.ruleId, status, assigneeId: assigneeId ?? undefined})
        .catch(err => logger.warn('Failed to persist assignee', err))
    }
  },

  loadBaseline: async flowId => {
    if (isTauri() || !flowId) return
    try {
      const [bl, drift] = await Promise.all([analysisApi.getBaseline(flowId), analysisApi.baselineDrift(flowId)])
      if (useFlowStore.getState().document?.id !== flowId) return
      const driftCount = drift.new.length - (drift.newFromNewRules ?? 0) - (drift.preview ?? 0)
      set({baseline: bl, baselineNewCount: drift.hasBaseline ? Math.max(0, driftCount) : null})
    } catch (err) {
      logger.warn('Failed to load baseline', err)
    }
  },

  handleSetBaseline: async () => {
    const flowId = useFlowStore.getState().document?.id
    if (!flowId) return
    try {
      await analysisApi.setBaseline(flowId)
      set({baseline: await analysisApi.getBaseline(flowId), baselineNewCount: 0})
    } catch (err) {
      logger.warn('Failed to set baseline', err)
      throw err
    }
  },

  handleClearBaseline: async () => {
    const flowId = useFlowStore.getState().document?.id
    if (!flowId) return
    try {
      await analysisApi.clearBaseline(flowId)
      set({baseline: null, baselineNewCount: null})
    } catch (err) {
      logger.warn('Failed to clear baseline', err)
      throw err
    }
  },

  saveCurrentView: (name, severities, hiddenCategories) => {
    const view: SavedFilterView = {
      name,
      severities: [...severities],
      hiddenCategories: [...hiddenCategories],
      categories: LEGACY_VIEW_CATEGORIES.filter(c => !hiddenCategories.has(c)),
    }
    set(state => {
      const views = [...state.savedViews.filter(v => v.name !== name), view]
      persistSavedViews(views)
      return {savedViews: views}
    })
  },

  deleteSavedView: name => {
    set(state => {
      const views = state.savedViews.filter(v => v.name !== name)
      persistSavedViews(views)
      return {savedViews: views}
    })
  },

  toggleFindingSelection: id => {
    set(state => {
      const ids = new Set(state.selectedFindingIds)
      if (ids.has(id)) ids.delete(id)
      else ids.add(id)
      return {selectedFindingIds: ids}
    })
  },

  selectAllFindings: ids => {
    set(state => {
      const current = state.selectedFindingIds
      const allSelected = ids.every(id => current.has(id))
      const next = new Set(current)
      if (allSelected) {
        for (const id of ids) next.delete(id)
      } else {
        for (const id of ids) next.add(id)
      }
      return {selectedFindingIds: next}
    })
  },

  clearFindingSelection: () => set({selectedFindingIds: new Set()}),

  setProtectedFlowId: id => set({protectedFlowId: id}),

  reset: () =>
    set({
      reports: new Map(),
      isAnalyzing: false,
      analyzingGen: 0,
      progress: {current: 0, total: 0, ruleName: ''},
      severityFilter: defaultSeverityFilter(),
      hiddenCategories: new Set(),
      statusFilter: defaultStatusFilter(),
      assignedToMeOnly: false,
      variableLineage: null,
      suppressedFindings: [],
      suppressedKeys: new Set(),
      findingSearch: '',
      protectedFlowId: null,
      triageMap: new Map(),
      baseline: null,
      baselineNewCount: null,
      selectedFindingIds: new Set(),
      focusedFindingKey: null,
    }),
}))

registerStoreReset(() => useAnalysisStore.getState().reset())
