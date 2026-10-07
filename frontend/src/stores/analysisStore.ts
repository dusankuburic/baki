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

export type FindingCategory = 'Security' | 'Reliability' | 'Performance' | 'Style' | 'Logic'

export interface SavedFilterView {
  name: string
  severities: Severity[]
  categories: FindingCategory[]
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
    // localStorage unavailable (private browsing) — views stay in-memory only
  }
}

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

// Per-block index of a report's findings, built on demand and cached per
// report object (a WeakMap, so it is collected with the report). The store
// used to keep an eager index for every retained report — a second copy of
// every findings array, for flows only the inspector of the OPEN flow reads
// — and rebuilt it on every setReport. Building one costs ~3 ms for 20k
// findings, once per report.
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
  categoryFilter: Set<FindingCategory>
  // Triage-queue filters (R0-3): status chips + "assigned to me" narrow the
  // list to the user's working set — resolved/acknowledged findings used to
  // stay fully visible with no way to hide them.
  statusFilter: Set<TriageStatus>
  assignedToMeOnly: boolean
  variableLineage: VariableHistory | null
  suppressedFindings: SuppressedFinding[]
  suppressedKeys: Set<string>
  findingSearch: string
  protectedFlowId: string | null
  // Triage (cloud mode): per-finding status map (acknowledged / in_progress /
  // resolved / suppressed). Suppressed entries are also mirrored in
  // suppressedKeys for the existing suppress UI.
  triageMap: Map<string, FindingStatus>
  baseline: FlowBaseline | null
  baselineNewCount: number | null
  savedViews: SavedFilterView[]
  selectedFindingIds: Set<string>
  // focusedFindingKey is a stable findingKey the findings list should scroll to
  // and briefly highlight — set by a chat "finding:" deep-link, cleared once
  // the list has revealed it.
  focusedFindingKey: string | null

  setReport: (flowId: string, report: AnalysisReport) => void
  setFocusedFinding: (key: string | null) => void
  setAnalyzing: (b: boolean) => void
  beginAnalyzing: () => number
  setProgress: (p: {current: number; total: number; ruleName: string}) => void
  toggleSeverityFilter: (s: Severity) => void
  setSeverityFilter: (s: Set<Severity>) => void
  toggleCategoryFilter: (c: FindingCategory) => void
  setCategoryFilter: (c: Set<FindingCategory>) => void
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
  // justification is the user-authored reason (prompted when suppressing);
  // stored on the triage entry so the status chip can show it.
  setFindingTriage: (finding: Finding, status: TriageStatus, justification?: string) => void
  // triageFindingsBatch applies one triage patch (status and/or assignee) to
  // many findings (bulk triage): optimistic triageMap update + ONE persisted
  // batch request. prev entries are snapshotted for rollback.
  triageFindingsBatch: (findings: Finding[], patch: {status?: TriageStatus; assigneeId?: string | null}) => void
  assignFinding: (finding: Finding, assigneeId: string | null) => void
  loadBaseline: (flowId: string) => Promise<void>
  handleSetBaseline: () => Promise<void>
  handleClearBaseline: () => Promise<void>
  saveCurrentView: (name: string, severities: Set<Severity>, categories: Set<FindingCategory>) => void
  deleteSavedView: (name: string) => void
  toggleFindingSelection: (id: string) => void
  selectAllFindings: (ids: string[]) => void
  clearFindingSelection: () => void
  setProtectedFlowId: (id: string | null) => void
  reset: () => void
}

// Retained reports are capped by count AND by the findings they hold: 20
// reports of a 20k-finding flow kept ~10 MB of JSON each alive per session.
// The open flow's report (protectedFlowId) and the report just set are never
// evicted, so the budget can be exceeded by exactly those two.
const MAX_REPORTS = 20
export const MAX_RETAINED_FINDINGS = 100_000

const defaultSeverityFilter = (): Set<Severity> => new Set(['error', 'warning', 'info'])
// All statuses on by default: 'suppressed' is listed for completeness, but
// suppressed findings are hidden by the suppress filter regardless.
const defaultStatusFilter = (): Set<TriageStatus> => new Set(['open', 'acknowledged', 'in_progress', 'resolved'])
const defaultCategoryFilter = (): Set<FindingCategory> =>
  new Set<FindingCategory>(['Security', 'Reliability', 'Performance', 'Style', 'Logic'])

export const useAnalysisStore = create<AnalysisState>((set, get) => ({
  reports: new Map(),
  isAnalyzing: false,
  analyzingGen: 0,
  progress: {current: 0, total: 0, ruleName: ''},
  severityFilter: defaultSeverityFilter(),
  categoryFilter: defaultCategoryFilter(),
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
      // Delete-then-set: a Map keeps a re-set key's ORIGINAL position, so a
      // refreshed report would otherwise still count as the oldest.
      next.delete(flowId)
      next.set(flowId, report)
      // Evict oldest entries beyond the count cap or the findings budget, but
      // never the currently-open flow's report — losing it causes the
      // per-block findings UI to silently go empty while the dashboard
      // aggregate still shows it — nor the one just set.
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
    // Best-effort notification when the user has tabbed away from the app.
    // notifyIfBackground is a no-op while the document is visible, so this
    // never disturbs an active session.
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

  toggleCategoryFilter: c =>
    set(state => ({
      categoryFilter: toggleSetMember(state.categoryFilter, c),
    })),

  setCategoryFilter: c => set({categoryFilter: new Set(c)}),

  toggleStatusFilter: (st: TriageStatus) =>
    set(state => ({
      statusFilter: toggleSetMember(state.statusFilter, st),
    })),

  setStatusFilter: (sts: Set<TriageStatus>) => set({statusFilter: new Set(sts)}),

  toggleAssignedToMe: () => set(state => ({assignedToMeOnly: !state.assignedToMeOnly})),

  setVariableLineage: h => set({variableLineage: h}),

  setFindingSearch: q => set({findingSearch: q}),

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
          // Roll back the optimistic update so local state matches the server.
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
      // Guard against a stale response: if the user switched flows while this
      // request was in flight, applying flowId's suppressions would clobber the
      // now-current flow's triage state.
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
    // Preserve any existing assignee across a status change so triaging a
    // finding (e.g. open → in_progress) doesn't silently drop its owner.
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
      // Keep the suppress filter in sync with the triage status: a finding is
      // hidden iff its status is 'suppressed'; any other status must reveal it.
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

  // Assign (or unassign) a finding without changing its triage status. The
  // backend persists assigneeId alongside the status, so we re-send the current
  // status with the new owner. Cloud-only (desktop triage is in-memory).
  // A finding whose stored status this bundle does not know is left as it is
  // when the patch keeps the status: requests send known statuses only.
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
        // Index into prev directly: indexOf(f) is O(n²) AND wrong
        // when the same object reference appears twice.
        const existing = prev[i]
        const status = patch.status ?? existing?.status ?? 'in_progress'
        if (!isKnown(TRIAGE_STATUSES, status)) return []
        const assigneeId = patch.assigneeId === undefined ? existing?.assigneeId : patch.assigneeId || undefined
        return [{findingKey: findingKey(f), ruleId: f.ruleId, status, assigneeId}]
      })
      if (items.length === 0) return
      analysisApi.setFindingStatusBatch(items).catch(err => {
        logger.warn('Failed to persist bulk triage', err)
        // Roll back: restore each entry's prior shape (or absence).
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
    // The request repeats the status, and a status this bundle does not know
    // cannot be sent (the triage menu offers no assignment then).
    if (!isKnown(TRIAGE_STATUSES, status)) return
    set(state => {
      const triageMap = new Map(state.triageMap)
      if (status === 'open' && !assigneeId) {
        // Unassigning a finding that was never triaged: nothing to persist.
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
      // Guard against a stale response overwriting the now-current flow's baseline.
      if (useFlowStore.getState().document?.id !== flowId) return
      set({baseline: bl, baselineNewCount: drift.hasBaseline ? drift.new.length : null})
    } catch (err) {
      logger.warn('Failed to load baseline', err)
    }
  },

  handleSetBaseline: async () => {
    const flowId = useFlowStore.getState().document?.id
    if (!flowId) return
    try {
      // `baselineNewCount` is definitionally 0 right after setting a baseline.
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

  saveCurrentView: (name, severities, categories) => {
    const view: SavedFilterView = {
      name,
      severities: [...severities],
      categories: [...categories],
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
      // If all given ids are already selected, deselect them (toggle-all)
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
      categoryFilter: defaultCategoryFilter(),
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
      // savedViews are DEVICE-LOCAL prefs (persisted to localStorage, loaded
      // at module init) — logout must not blank them for the session:
      // they'd stay gone until a reload despite surviving on disk.
      focusedFindingKey: null,
    }),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => useAnalysisStore.getState().reset())
