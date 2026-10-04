import {describe, it, expect, beforeEach, vi} from 'vitest'
import {useAnalysisStore, findingKey, findingsByBlockOf, MAX_RETAINED_FINDINGS} from './analysisStore'
import type {Finding, AnalysisReport} from '@/types'

// Suppression persists via the triage API in cloud mode (jsdom reports !isTauri).
// Mock it so the store's optimistic local behavior is what's under test.
vi.mock('@/api', () => ({
  analysisApi: {
    setFindingStatus: vi.fn().mockResolvedValue({}),
    setFindingStatusBatch: vi.fn().mockResolvedValue({updated: 0}),
    clearFindingStatus: vi.fn().mockResolvedValue(undefined),
    listFindingStatuses: vi.fn().mockResolvedValue([]),
  },
}))

import {analysisApi} from '@/api'

function makeFinding(id: string, over: Partial<Finding> = {}): Finding {
  return {
    id,
    ruleId: 'unused-variable',
    severity: 'info',
    title: 'Variable declared but never used',
    description: 'desc',
    blockId: 'b1',
    subflowId: 'sf1',
    suggestion: '',
    category: 'Style',
    ...over,
  } as Finding
}

// The findings filters read suppressedKeys directly, so that set is what
// "suppressed" means to the UI.
function isSuppressed(f: Finding): boolean {
  return useAnalysisStore.getState().suppressedKeys.has(findingKey(f))
}

beforeEach(() => {
  useAnalysisStore.getState().reset()
})

describe('suppression', () => {
  // Findings are keyed by ruleId:blockId, so distinct findings need distinct blocks.
  const f1 = () => makeFinding('f1', {blockId: 'b1'})
  const f2 = () => makeFinding('f2', {blockId: 'b2'})
  const f3 = () => makeFinding('f3', {blockId: 'b3'})

  it('suppressFinding marks a finding suppressed', () => {
    useAnalysisStore.getState().suppressFinding(f1(), 'noise')
    expect(isSuppressed(f1())).toBe(true)
    expect(isSuppressed(f2())).toBe(false)
  })

  it('suppression is keyed by fingerprint, so it survives a finding.id change', () => {
    useAnalysisStore.getState().suppressFinding(makeFinding('F-001', {blockId: 'b1'}), 'noise')
    // A later analysis renumbers ids but the same rule fires on the same block.
    expect(isSuppressed(makeFinding('F-042', {blockId: 'b1'}))).toBe(true)
  })

  it('suppressMany suppresses a whole group and skips already-suppressed', () => {
    const s = useAnalysisStore.getState()
    s.suppressFinding(f1(), 'noise')
    s.suppressMany([f1(), f2(), f3()], 'bulk')

    const state = useAnalysisStore.getState()
    expect(isSuppressed(f1())).toBe(true)
    expect(isSuppressed(f2())).toBe(true)
    expect(isSuppressed(f3())).toBe(true)
    // f1 must not be duplicated in the list
    expect(state.suppressedFindings.filter(x => x.key === findingKey(f1()))).toHaveLength(1)
  })

  it('suppressMany persists the batch in a single request', () => {
    vi.mocked(analysisApi.setFindingStatusBatch).mockClear()
    useAnalysisStore.getState().suppressMany([f1(), f2(), f3()], 'bulk')
    // One HTTP call for the whole batch, not one per finding.
    expect(analysisApi.setFindingStatusBatch).toHaveBeenCalledTimes(1)
    const items = vi.mocked(analysisApi.setFindingStatusBatch).mock.calls[0][0]
    expect(items).toHaveLength(3)
    expect(items.every(i => i.status === 'suppressed')).toBe(true)
  })

  it('unsuppressFinding restores a single finding', () => {
    useAnalysisStore.getState().suppressMany([f1(), f2()], 'bulk')
    useAnalysisStore.getState().unsuppressFinding(f1())
    expect(isSuppressed(f1())).toBe(false)
    expect(isSuppressed(f2())).toBe(true)
  })

  it('clearSuppressed restores everything', () => {
    useAnalysisStore.getState().suppressMany([f1(), f2()], 'bulk')
    useAnalysisStore.getState().clearSuppressed()
    expect(useAnalysisStore.getState().suppressedFindings).toHaveLength(0)
  })
})

describe('filters', () => {
  it('toggleSeverityFilter removes and re-adds a severity', () => {
    useAnalysisStore.getState().toggleSeverityFilter('error')
    expect(useAnalysisStore.getState().severityFilter.has('error')).toBe(false)
    useAnalysisStore.getState().toggleSeverityFilter('error')
    expect(useAnalysisStore.getState().severityFilter.has('error')).toBe(true)
  })

  it('toggleCategoryFilter removes and re-adds a category', () => {
    useAnalysisStore.getState().toggleCategoryFilter('Security')
    expect(useAnalysisStore.getState().categoryFilter.has('Security')).toBe(false)
    useAnalysisStore.getState().toggleCategoryFilter('Security')
    expect(useAnalysisStore.getState().categoryFilter.has('Security')).toBe(true)
  })
})

describe('findingsByBlockOf', () => {
  const report = () =>
    ({
      flowId: 'doc1',
      findings: [
        makeFinding('f1', {blockId: 'b1'}),
        makeFinding('f2', {blockId: 'b2'}),
        makeFinding('f3', {blockId: 'b1'}),
      ],
    }) as unknown as AnalysisReport

  it('indexes a report by block for the inspector', () => {
    useAnalysisStore.getState().setReport('doc1', report())
    const index = findingsByBlockOf(useAnalysisStore.getState().reports.get('doc1'))
    expect((index.get('b1') ?? []).map(f => f.id)).toEqual(['f1', 'f3'])
    expect(index.get('b9')).toBeUndefined()
    expect(findingsByBlockOf(undefined).size).toBe(0)
  })

  // Built on demand, once per report object: the store no longer carries an
  // eager index for every retained report.
  it('is cached per report object and rebuilt for a new report', () => {
    const r1 = report()
    expect(findingsByBlockOf(r1)).toBe(findingsByBlockOf(r1))
    expect(findingsByBlockOf(report())).not.toBe(findingsByBlockOf(r1))
    expect(useAnalysisStore.getState()).not.toHaveProperty('findingsByBlock')
  })
})

describe('setFocusedFinding', () => {
  it('sets and clears the focused finding key', () => {
    useAnalysisStore.getState().setFocusedFinding('rule-x:blk-1')
    expect(useAnalysisStore.getState().focusedFindingKey).toBe('rule-x:blk-1')
    useAnalysisStore.getState().setFocusedFinding(null)
    expect(useAnalysisStore.getState().focusedFindingKey).toBeNull()
  })
})

describe('setReport eviction', () => {
  function makeReport(flowId: string): AnalysisReport {
    return {flowId, findings: []} as unknown as AnalysisReport
  }

  it('evicts the oldest entry beyond MAX_REPORTS', () => {
    const store = useAnalysisStore.getState()
    for (let i = 0; i < 20; i++) {
      store.setReport(`flow-${i}`, makeReport(`flow-${i}`))
    }
    expect(useAnalysisStore.getState().reports.size).toBe(20)
    store.setReport('flow-20', makeReport('flow-20'))
    expect(useAnalysisStore.getState().reports.size).toBe(20)
    expect(useAnalysisStore.getState().reports.has('flow-0')).toBe(false)
    expect(useAnalysisStore.getState().reports.has('flow-20')).toBe(true)
  })

  it('never evicts the protectedFlowId', () => {
    const store = useAnalysisStore.getState()
    store.setProtectedFlowId('flow-0')
    for (let i = 0; i < 21; i++) {
      store.setReport(`flow-${i}`, makeReport(`flow-${i}`))
    }
    expect(useAnalysisStore.getState().reports.size).toBe(20)
    expect(useAnalysisStore.getState().reports.has('flow-0')).toBe(true)
    expect(useAnalysisStore.getState().reports.has('flow-1')).toBe(false)
  })

  function makeBigReport(flowId: string, findings: number): AnalysisReport {
    const list: Finding[] = new Array(findings)
    for (let i = 0; i < findings; i++) list[i] = makeFinding(`${flowId}-${i}`, {blockId: `b${i}`})
    return {flowId, findings: list} as unknown as AnalysisReport
  }

  // 20 reports of a 20k-finding flow kept ~10 MB of JSON each alive per
  // session: the cap is now also a findings budget.
  it('evicts the oldest non-active report once the retained findings exceed the budget', () => {
    const store = useAnalysisStore.getState()
    const half = Math.ceil(MAX_RETAINED_FINDINGS / 2)
    store.setProtectedFlowId('active')
    store.setReport('active', makeBigReport('active', half))
    store.setReport('old', makeBigReport('old', half - 1))
    expect(useAnalysisStore.getState().reports.size).toBe(2) // within budget

    store.setReport('newer', makeBigReport('newer', 10))

    const reports = useAnalysisStore.getState().reports
    expect(reports.has('active')).toBe(true) // the open flow's report survives
    expect(reports.has('newer')).toBe(true) // the one just set survives
    expect(reports.has('old')).toBe(false) // the oldest non-active one went
  })

  it('a refreshed report becomes the newest, not the oldest, entry', () => {
    const store = useAnalysisStore.getState()
    for (let i = 0; i < 20; i++) store.setReport(`flow-${i}`, makeReport(`flow-${i}`))
    store.setReport('flow-0', makeReport('flow-0')) // refreshed: moves to the newest slot
    store.setReport('flow-20', makeReport('flow-20'))
    expect(useAnalysisStore.getState().reports.has('flow-0')).toBe(true)
    expect(useAnalysisStore.getState().reports.has('flow-1')).toBe(false)
  })
})

describe('reset', () => {
  it('clears every field back to defaults', () => {
    const store = useAnalysisStore.getState()
    store.setReport('doc1', {flowId: 'doc1', findings: []} as unknown as AnalysisReport)
    store.suppressFinding(makeFinding('f1'), 'noise')
    store.setAnalyzing(true)
    store.beginAnalyzing()
    store.setProgress({current: 5, total: 10, ruleName: 'test'})
    store.setVariableLineage({} as never)
    store.setFindingSearch('query')
    store.toggleSeverityFilter('error')
    store.setProtectedFlowId('doc1')

    useAnalysisStore.getState().reset()

    const s = useAnalysisStore.getState()
    expect(s.reports.size).toBe(0)
    expect(s.suppressedFindings).toHaveLength(0)
    expect(s.suppressedKeys.size).toBe(0)
    expect(s.isAnalyzing).toBe(false)
    expect(s.analyzingGen).toBe(0)
    expect(s.progress).toEqual({current: 0, total: 0, ruleName: ''})
    expect(s.variableLineage).toBeNull()
    expect(s.findingSearch).toBe('')
    expect(s.protectedFlowId).toBeNull()
    expect(s.severityFilter.has('error')).toBe(true)
    expect(s.severityFilter.has('warning')).toBe(true)
    expect(s.severityFilter.has('info')).toBe(true)
  })
})

describe('saved filter views', () => {
  it('saveCurrentView stores the current severity + category filters', () => {
    const s = useAnalysisStore.getState()
    s.setSeverityFilter(new Set(['error', 'warning']))
    s.saveCurrentView('Errors+Warnings', new Set(['error', 'warning']), new Set(['Security']))
    const views = useAnalysisStore.getState().savedViews
    expect(views.some(v => v.name === 'Errors+Warnings')).toBe(true)
    const view = views.find(v => v.name === 'Errors+Warnings')!
    expect(view.severities).toEqual(['error', 'warning'])
    expect(view.categories).toEqual(['Security'])
  })

  it('saveCurrentView replaces an existing view with the same name', () => {
    const s = useAnalysisStore.getState()
    s.saveCurrentView('MyView', new Set(['error']), new Set(['Security']))
    s.saveCurrentView('MyView', new Set(['warning', 'info']), new Set(['Reliability']))
    const views = useAnalysisStore.getState().savedViews
    expect(views.filter(v => v.name === 'MyView')).toHaveLength(1)
    expect(views.find(v => v.name === 'MyView')!.severities).toEqual(['warning', 'info'])
  })

  it('deleteSavedView removes a view by name', () => {
    const s = useAnalysisStore.getState()
    s.saveCurrentView('ToDelete', new Set(['error']), new Set(['Security']))
    expect(useAnalysisStore.getState().savedViews.some(v => v.name === 'ToDelete')).toBe(true)
    useAnalysisStore.getState().deleteSavedView('ToDelete')
    expect(useAnalysisStore.getState().savedViews.some(v => v.name === 'ToDelete')).toBe(false)
  })
})

describe('finding triage status', () => {
  it('setFindingTriage stores non-open status in triageMap', () => {
    const f = makeFinding('f1', {blockId: 'b1'})
    useAnalysisStore.getState().setFindingTriage(f, 'acknowledged')
    const key = findingKey(f)
    const triage = useAnalysisStore.getState().triageMap.get(key)
    expect(triage).toBeDefined()
    expect(triage!.status).toBe('acknowledged')
  })

  it('setFindingTriage with open removes the entry from triageMap', () => {
    const f = makeFinding('f1', {blockId: 'b1'})
    useAnalysisStore.getState().setFindingTriage(f, 'resolved')
    useAnalysisStore.getState().setFindingTriage(f, 'open')
    const key = findingKey(f)
    expect(useAnalysisStore.getState().triageMap.has(key)).toBe(false)
  })

  it('setFindingTriage persists via setFindingStatus in cloud mode', () => {
    vi.mocked(analysisApi.setFindingStatus).mockClear()
    const f = makeFinding('f1', {blockId: 'b1'})
    useAnalysisStore.getState().setFindingTriage(f, 'in_progress')
    expect(analysisApi.setFindingStatus).toHaveBeenCalledTimes(1)
  })

  // Regression: choosing "Suppressed" from the triage menu must also hide the
  // finding (the suppress filter reads suppressedKeys), not just record the
  // triage status. Previously the finding stayed visible until a reload.
  it('setFindingTriage suppressed mirrors into suppressedKeys and hides the finding', () => {
    const f = makeFinding('f1', {blockId: 'b1'})
    useAnalysisStore.getState().setFindingTriage(f, 'suppressed')
    const key = findingKey(f)
    expect(useAnalysisStore.getState().suppressedKeys.has(key)).toBe(true)
    expect(isSuppressed(f)).toBe(true)
    expect(useAnalysisStore.getState().suppressedFindings.find(s => s.key === key)).toBeDefined()
  })

  it('setFindingTriage from suppressed to resolved reveals the finding again', () => {
    const f = makeFinding('f1', {blockId: 'b1'})
    useAnalysisStore.getState().setFindingTriage(f, 'suppressed')
    expect(isSuppressed(f)).toBe(true)

    useAnalysisStore.getState().setFindingTriage(f, 'resolved')
    const key = findingKey(f)
    expect(useAnalysisStore.getState().suppressedKeys.has(key)).toBe(false)
    expect(isSuppressed(f)).toBe(false)
    expect(useAnalysisStore.getState().suppressedFindings.find(s => s.key === key)).toBeUndefined()
    // The triage status itself is still recorded.
    expect(useAnalysisStore.getState().triageMap.get(key)?.status).toBe('resolved')
  })
})

describe('finding assignment', () => {
  const f = () => makeFinding('f1', {blockId: 'b1'})

  beforeEach(() => {
    vi.mocked(analysisApi.setFindingStatus).mockClear()
  })

  it('assignFinding sets the assignee on an open finding', () => {
    useAnalysisStore.getState().assignFinding(f(), 'user-7')
    const key = findingKey(f())
    const entry = useAnalysisStore.getState().triageMap.get(key)
    expect(entry?.assigneeId).toBe('user-7')
    // Status defaults to 'open' since none was set.
    expect(entry?.status).toBe('open')
    // Persisted with the assignee id.
    expect(analysisApi.setFindingStatus).toHaveBeenCalledWith(expect.objectContaining({assigneeId: 'user-7'}))
  })

  it('assignFinding preserves the existing triage status', () => {
    useAnalysisStore.getState().setFindingTriage(f(), 'in_progress')
    vi.mocked(analysisApi.setFindingStatus).mockClear()

    useAnalysisStore.getState().assignFinding(f(), 'user-9')
    const key = findingKey(f())
    expect(useAnalysisStore.getState().triageMap.get(key)?.status).toBe('in_progress')
    expect(useAnalysisStore.getState().triageMap.get(key)?.assigneeId).toBe('user-9')
    // The persist call carries BOTH the preserved status and the new assignee.
    expect(analysisApi.setFindingStatus).toHaveBeenCalledWith(
      expect.objectContaining({status: 'in_progress', assigneeId: 'user-9'}),
    )
  })

  it('setFindingTriage preserves an existing assignee across a status change', () => {
    useAnalysisStore.getState().setFindingTriage(f(), 'acknowledged')
    useAnalysisStore.getState().assignFinding(f(), 'user-2')

    // Now change status — the assignee must survive.
    useAnalysisStore.getState().setFindingTriage(f(), 'in_progress')
    const key = findingKey(f())
    expect(useAnalysisStore.getState().triageMap.get(key)?.status).toBe('in_progress')
    expect(useAnalysisStore.getState().triageMap.get(key)?.assigneeId).toBe('user-2')
  })

  it('assignFinding(null) unassigns', () => {
    useAnalysisStore.getState().setFindingTriage(f(), 'in_progress')
    useAnalysisStore.getState().assignFinding(f(), 'user-2')
    useAnalysisStore.getState().assignFinding(f(), null)
    const key = findingKey(f())
    expect(useAnalysisStore.getState().triageMap.get(key)?.assigneeId).toBeUndefined()
    // Status is untouched.
    expect(useAnalysisStore.getState().triageMap.get(key)?.status).toBe('in_progress')
  })
})

describe('bulk finding selection', () => {
  it('toggleFindingSelection adds and removes a finding', () => {
    useAnalysisStore.getState().toggleFindingSelection('f1')
    expect(useAnalysisStore.getState().selectedFindingIds.has('f1')).toBe(true)
    useAnalysisStore.getState().toggleFindingSelection('f1')
    expect(useAnalysisStore.getState().selectedFindingIds.has('f1')).toBe(false)
  })

  it('selectAllFindings toggles all given ids', () => {
    const s = useAnalysisStore.getState()
    s.selectAllFindings(['f1', 'f2', 'f3'])
    expect(useAnalysisStore.getState().selectedFindingIds.size).toBe(3)
    // Toggle again deselects all
    useAnalysisStore.getState().selectAllFindings(['f1', 'f2', 'f3'])
    expect(useAnalysisStore.getState().selectedFindingIds.size).toBe(0)
  })

  it('clearFindingSelection empties the set', () => {
    useAnalysisStore.getState().selectAllFindings(['f1', 'f2'])
    useAnalysisStore.getState().clearFindingSelection()
    expect(useAnalysisStore.getState().selectedFindingIds.size).toBe(0)
  })
})

describe('triageFindingsBatch (U4.4)', () => {
  const f1 = () => makeFinding('f1', {blockId: 'b1'})
  const f2 = () => makeFinding('f2', {blockId: 'b2'})

  it('applies status + assignee optimistically and persists ONE batch request', () => {
    vi.mocked(analysisApi.setFindingStatusBatch).mockClear()
    useAnalysisStore.getState().triageFindingsBatch([f1(), f2()], {status: 'in_progress', assigneeId: 'me'})
    const st = useAnalysisStore.getState()
    expect(st.triageMap.get(findingKey(f1()))?.assigneeId).toBe('me')
    expect(st.triageMap.get(findingKey(f2()))?.status).toBe('in_progress')
    expect(vi.mocked(analysisApi.setFindingStatusBatch)).toHaveBeenCalledTimes(1)
    const items = vi.mocked(analysisApi.setFindingStatusBatch).mock.calls[0][0]
    expect(items).toHaveLength(2)
    expect(items.every(i => i.assigneeId === 'me' && i.status === 'in_progress')).toBe(true)
  })

  it('rolls back the optimistic update when the batch fails', async () => {
    vi.mocked(analysisApi.setFindingStatusBatch).mockClear()
    vi.mocked(analysisApi.setFindingStatusBatch).mockRejectedValueOnce(new Error('boom'))
    useAnalysisStore.getState().triageFindingsBatch([f1()], {status: 'resolved'})
    // Optimistic first...
    expect(useAnalysisStore.getState().triageMap.get(findingKey(f1()))?.status).toBe('resolved')
    // ...then rolled back after the rejection settles.
    await vi.waitFor(() => {
      expect(useAnalysisStore.getState().triageMap.get(findingKey(f1()))?.status ?? 'open').toBe('open')
    })
  })
})
