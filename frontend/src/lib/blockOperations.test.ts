import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {FlowDocument} from '@/types'

const moveBlock = vi.fn()
const listSnapshots = vi.fn()
const analyzeFlowById = vi.fn()
vi.mock('@/api', () => ({
  flowApi: {
    moveBlock: (...a: unknown[]) => moveBlock(...a),
    listSnapshots: (...a: unknown[]) => listSnapshots(...a),
  },
  analysisApi: {
    analyzeFlowById: (...a: unknown[]) => analyzeFlowById(...a),
    setFindingStatus: vi.fn().mockResolvedValue({}),
    setFindingStatusBatch: vi.fn().mockResolvedValue({updated: 0}),
  },
}))

const toast = {success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn()}
vi.mock('@/components/shared', () => ({
  useToast: () => toast,
  useConfirm: () => ({confirm: vi.fn().mockResolvedValue(true)}),
}))

import {useBlockOperations} from './blockOperations'
import {REANALYZE_DEBOUNCE_MS} from './blockEdit'

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

const flush = () =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })

beforeEach(() => {
  vi.useFakeTimers()
  vi.clearAllMocks()
  listSnapshots.mockResolvedValue({snapshots: [{id: 'snap-0', label: '', createdAt: '', bytes: 1}]})
  analyzeFlowById.mockResolvedValue({flowId: 'flow-1', findings: []})
  useFlowStore.setState({document: doc('v0')} as never)
  useAnalysisStore.getState().reset()
})
afterEach(() => vi.useRealTimers())

describe('useBlockOperations.moveBlock', () => {
  // Alt+Up/Down fires one moveBlock per keypress. Three presses before the
  // first response used to mean three concurrent requests, three applied
  // documents, three re-analyses.
  it('three moves before the first resolves: one request in flight, the LAST document applied, one re-analysis', async () => {
    const d1 = deferred<{document: FlowDocument}>()
    const d2 = deferred<{document: FlowDocument}>()
    const d3 = deferred<{document: FlowDocument}>()
    moveBlock.mockReturnValueOnce(d1.promise).mockReturnValueOnce(d2.promise).mockReturnValueOnce(d3.promise)
    const {result} = renderHook(() => useBlockOperations())

    const calls: Promise<void>[] = []
    act(() => {
      calls.push(result.current.moveBlock('b1', 'down'))
      calls.push(result.current.moveBlock('b1', 'down'))
      calls.push(result.current.moveBlock('b1', 'down'))
    })
    await flush()
    expect(moveBlock).toHaveBeenCalledTimes(1)

    d1.resolve({document: doc('v1')})
    await flush()
    expect(moveBlock).toHaveBeenCalledTimes(2)
    expect(moveBlock).toHaveBeenLastCalledWith('flow-1', 'b1', 'down')

    d2.resolve({document: doc('v2')})
    await flush()
    expect(moveBlock).toHaveBeenCalledTimes(3)

    d3.resolve({document: doc('v3')})
    await flush()
    await act(() => Promise.all(calls))

    expect(useFlowStore.getState().document?.name).toBe('v3')
    expect(toast.error).not.toHaveBeenCalled()
    expect(analyzeFlowById).not.toHaveBeenCalled()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(REANALYZE_DEBOUNCE_MS)
    })
    expect(analyzeFlowById).toHaveBeenCalledTimes(1)
    expect(analyzeFlowById).toHaveBeenCalledWith('flow-1')
  })

  it('captures the undo snapshot right before its own request and offers it on the toast', async () => {
    moveBlock.mockResolvedValue({document: doc('v1')})
    const {result} = renderHook(() => useBlockOperations())
    await act(() => result.current.moveBlock('b1', 'up'))

    expect(listSnapshots).toHaveBeenCalledWith('flow-1')
    expect(toast.success).toHaveBeenCalledWith('Block moved up', expect.objectContaining({action: expect.anything()}))
    expect(useFlowStore.getState().document?.name).toBe('v1')
  })

  it('surfaces a refused move as a toast without touching the document', async () => {
    moveBlock.mockRejectedValue(new Error('block is already first'))
    const {result} = renderHook(() => useBlockOperations())
    await act(() => result.current.moveBlock('b1', 'up'))

    expect(toast.error).toHaveBeenCalledWith('Move failed', expect.objectContaining({description: expect.any(String)}))
    expect(useFlowStore.getState().document?.name).toBe('v0')
  })
})
