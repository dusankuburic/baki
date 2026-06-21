import {describe, it, expect, beforeEach, vi} from 'vitest'
import {useAnalysisStore, findingKey} from './analysisStore'
import type {Finding, AnalysisReport} from '@/types'

// Suppression persists via the triage API in cloud mode (jsdom reports !isTauri).
// Mock it so the store's optimistic local behavior is what's under test.
vi.mock('@/api', () => ({
  analysisApi: {
    setFindingStatus: vi.fn().mockResolvedValue({}),
    clearFindingStatus: vi.fn().mockResolvedValue(undefined),
    listFindingStatuses: vi.fn().mockResolvedValue([]),
  },
}))

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
        expect(useAnalysisStore.getState().isSuppressed(f1())).toBe(true)
        expect(useAnalysisStore.getState().isSuppressed(f2())).toBe(false)
    })

    it('suppression is keyed by fingerprint, so it survives a finding.id change', () => {
        useAnalysisStore.getState().suppressFinding(makeFinding('F-001', {blockId: 'b1'}), 'noise')
        // A later analysis renumbers ids but the same rule fires on the same block.
        expect(useAnalysisStore.getState().isSuppressed(makeFinding('F-042', {blockId: 'b1'}))).toBe(true)
    })

    it('suppressMany suppresses a whole group and skips already-suppressed', () => {
        const s = useAnalysisStore.getState()
        s.suppressFinding(f1(), 'noise')
        s.suppressMany([f1(), f2(), f3()], 'bulk')

        const state = useAnalysisStore.getState()
        expect(state.isSuppressed(f1())).toBe(true)
        expect(state.isSuppressed(f2())).toBe(true)
        expect(state.isSuppressed(f3())).toBe(true)
        // f1 must not be duplicated in the list
        expect(state.suppressedFindings.filter(x => x.key === findingKey(f1()))).toHaveLength(1)
    })

    it('unsuppressFinding restores a single finding', () => {
        useAnalysisStore.getState().suppressMany([f1(), f2()], 'bulk')
        useAnalysisStore.getState().unsuppressFinding(f1())
        expect(useAnalysisStore.getState().isSuppressed(f1())).toBe(false)
        expect(useAnalysisStore.getState().isSuppressed(f2())).toBe(true)
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

describe('findingsForBlock', () => {
    it('returns only the findings for the given block', () => {
        const report = {
            flowId: 'doc1',
            findings: [
                makeFinding('f1', {blockId: 'b1'}),
                makeFinding('f2', {blockId: 'b2'}),
                makeFinding('f3', {blockId: 'b1'}),
            ],
        } as unknown as AnalysisReport
        useAnalysisStore.getState().setReport('doc1', report)

        const got = useAnalysisStore.getState().findingsForBlock('doc1', 'b1')
        expect(got.map(f => f.id)).toEqual(['f1', 'f3'])
        expect(useAnalysisStore.getState().findingsForBlock('missing', 'b1')).toEqual([])
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
        expect(s.findingsByBlock.size).toBe(0)
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
