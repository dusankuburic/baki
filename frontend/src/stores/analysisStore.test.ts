import {describe, it, expect, beforeEach} from 'vitest'
import {useAnalysisStore} from './analysisStore'
import type {Finding, AnalysisReport} from '@/types'

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
    useAnalysisStore.setState({
        reports: new Map(),
        suppressedFindings: [],
        severityFilter: new Set(['error', 'warning', 'info']),
        categoryFilter: new Set(['Security', 'Reliability', 'Performance', 'Style', 'Logic']),
        findingSearch: '',
    })
})

describe('suppression', () => {
    it('suppressFinding marks a finding suppressed', () => {
        const s = useAnalysisStore.getState()
        s.suppressFinding(makeFinding('f1'), 'noise')
        expect(useAnalysisStore.getState().isSuppressed('f1')).toBe(true)
        expect(useAnalysisStore.getState().isSuppressed('f2')).toBe(false)
    })

    it('suppressMany suppresses a whole group and skips already-suppressed ids', () => {
        const s = useAnalysisStore.getState()
        s.suppressFinding(makeFinding('f1'), 'noise')
        s.suppressMany([makeFinding('f1'), makeFinding('f2'), makeFinding('f3')], 'bulk')

        const state = useAnalysisStore.getState()
        expect(state.isSuppressed('f1')).toBe(true)
        expect(state.isSuppressed('f2')).toBe(true)
        expect(state.isSuppressed('f3')).toBe(true)
        // f1 must not be duplicated in the list
        expect(state.suppressedFindings.filter(x => x.findingId === 'f1')).toHaveLength(1)
    })

    it('unsuppressFinding restores a single finding', () => {
        const s = useAnalysisStore.getState()
        s.suppressMany([makeFinding('f1'), makeFinding('f2')], 'bulk')
        useAnalysisStore.getState().unsuppressFinding('f1')
        expect(useAnalysisStore.getState().isSuppressed('f1')).toBe(false)
        expect(useAnalysisStore.getState().isSuppressed('f2')).toBe(true)
    })

    it('clearSuppressed restores everything', () => {
        const s = useAnalysisStore.getState()
        s.suppressMany([makeFinding('f1'), makeFinding('f2')], 'bulk')
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
