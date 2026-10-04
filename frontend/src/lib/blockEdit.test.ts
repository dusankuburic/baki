import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {FlowDocument} from '@/types'

const analyzeFlowById = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {
    analyzeFlowById: (...a: unknown[]) => analyzeFlowById(...a),
    setFindingStatus: vi.fn().mockResolvedValue({}),
    setFindingStatusBatch: vi.fn().mockResolvedValue({updated: 0}),
  },
}))

import {refreshAfterBlockEdit, runBlockEdit, REANALYZE_DEBOUNCE_MS, MAX_QUEUED_EDITS} from './blockEdit'

const doc = (name: string) => ({id: 'flow-1', name, subflows: []}) as unknown as FlowDocument

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return {promise, resolve, reject}
}

const flush = () => vi.advanceTimersByTimeAsync(0)

beforeEach(() => {
  vi.useFakeTimers()
  vi.clearAllMocks()
  analyzeFlowById.mockResolvedValue({flowId: 'flow-1', findings: []})
  useFlowStore.setState({document: doc('v0')} as never)
  useAnalysisStore.getState().reset()
})
afterEach(() => vi.useRealTimers())

describe('refreshAfterBlockEdit re-analysis debounce', () => {
  it('two refreshes within the window re-analyse once, after the window', async () => {
    refreshAfterBlockEdit(doc('v1'))
    refreshAfterBlockEdit(doc('v2'))
    expect(useFlowStore.getState().document?.name).toBe('v2')
    expect(analyzeFlowById).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(REANALYZE_DEBOUNCE_MS - 1)
    expect(analyzeFlowById).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(analyzeFlowById).toHaveBeenCalledTimes(1)
    expect(analyzeFlowById).toHaveBeenCalledWith('flow-1')
    await flush()
    expect(useAnalysisStore.getState().reports.has('flow-1')).toBe(true)
  })

  it('still guards the doc-switch race', async () => {
    refreshAfterBlockEdit({id: 'other', name: 'x', subflows: []} as unknown as FlowDocument)
    expect(useFlowStore.getState().document?.name).toBe('v0')
    await vi.advanceTimersByTimeAsync(REANALYZE_DEBOUNCE_MS)
    expect(analyzeFlowById).not.toHaveBeenCalled()
  })
})

describe('runBlockEdit lane', () => {
  it('keeps one edit in flight per flow and applies only the last document of a burst', async () => {
    const d1 = deferred<{document: FlowDocument}>()
    const d2 = deferred<{document: FlowDocument}>()
    const d3 = deferred<{document: FlowDocument}>()
    const edit = vi.fn().mockReturnValueOnce(d1.promise).mockReturnValueOnce(d2.promise).mockReturnValueOnce(d3.promise)

    const p1 = runBlockEdit('flow-1', edit)
    const p2 = runBlockEdit('flow-1', edit)
    const p3 = runBlockEdit('flow-1', edit)
    await flush()
    expect(edit).toHaveBeenCalledTimes(1)

    d1.resolve({document: doc('v1')})
    await flush()
    expect(edit).toHaveBeenCalledTimes(2)
    expect(useFlowStore.getState().document?.name).toBe('v0') // superseded: not applied

    d2.resolve({document: doc('v2')})
    await flush()
    expect(edit).toHaveBeenCalledTimes(3)
    expect(useFlowStore.getState().document?.name).toBe('v0')

    d3.resolve({document: doc('v3')})
    await flush()
    await Promise.all([p1, p2, p3])
    expect(useFlowStore.getState().document?.name).toBe('v3')

    await vi.advanceTimersByTimeAsync(REANALYZE_DEBOUNCE_MS)
    expect(analyzeFlowById).toHaveBeenCalledTimes(1)
  })

  it('a failed later edit still applies the newest successful document and rejects to its caller', async () => {
    const d1 = deferred<{document: FlowDocument}>()
    const d2 = deferred<{document: FlowDocument}>()
    const edit = vi.fn().mockReturnValueOnce(d1.promise).mockReturnValueOnce(d2.promise)

    const p1 = runBlockEdit('flow-1', edit)
    const p2 = runBlockEdit('flow-1', edit)
    d1.resolve({document: doc('v1')})
    await flush()
    expect(useFlowStore.getState().document?.name).toBe('v0')

    d2.reject(new Error('block is already last'))
    await expect(p2).rejects.toThrow('block is already last')
    await expect(p1).resolves.toEqual({document: doc('v1')})
    expect(useFlowStore.getState().document?.name).toBe('v1')
  })

  it('lanes are per flow: edits on different flows run concurrently', async () => {
    const d1 = deferred<{document: FlowDocument}>()
    const edit1 = vi.fn().mockReturnValue(d1.promise)
    const edit2 = vi.fn().mockResolvedValue({document: {id: 'flow-2', name: 'z', subflows: []}})
    void runBlockEdit('flow-1', edit1)
    void runBlockEdit('flow-2', edit2)
    await flush()
    expect(edit1).toHaveBeenCalledTimes(1)
    expect(edit2).toHaveBeenCalledTimes(1)
    d1.resolve({document: doc('v1')})
    await flush()
  })

  it('drops edits beyond the queue cap instead of letting a held key run away', async () => {
    const d1 = deferred<{document: FlowDocument}>()
    const edit = vi
      .fn()
      .mockReturnValueOnce(d1.promise)
      .mockResolvedValue({document: doc('vN')})

    const runs = [runBlockEdit('flow-1', edit)] // in flight
    for (let i = 0; i < MAX_QUEUED_EDITS; i++) runs.push(runBlockEdit('flow-1', edit)) // fills the queue
    const dropped = runBlockEdit('flow-1', edit)
    await expect(dropped).resolves.toBeUndefined()
    expect(edit).toHaveBeenCalledTimes(1)

    d1.resolve({document: doc('v1')})
    await Promise.all(runs)
    expect(edit).toHaveBeenCalledTimes(1 + MAX_QUEUED_EDITS)
    expect(useFlowStore.getState().document?.name).toBe('vN')
  })
})
