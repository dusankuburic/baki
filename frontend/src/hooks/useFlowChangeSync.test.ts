import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument} from '@/types'

const presence = vi.hoisted(() => ({
  listeners: [] as Array<(version: number) => void>,
  state: {flowId: 'flow-1' as string | null, status: 'connected'},
  emit(version: number) {
    for (const fn of [...presence.listeners]) fn(version)
  },
}))

vi.mock('@/stores/presenceStore', () => ({
  onFlowChanged: (cb: (version: number) => void) => {
    presence.listeners.push(cb)
    return () => {
      presence.listeners = presence.listeners.filter(fn => fn !== cb)
    }
  },
  usePresenceStore: (sel: (s: {flowId: string | null; status: string}) => unknown) => sel(presence.state),
}))

const get = vi.fn()
const getContent = vi.fn()
vi.mock('@/api/library', () => ({
  libraryApi: {
    get: (...a: unknown[]) => get(...a),
    getContent: (...a: unknown[]) => getContent(...a),
  },
}))

vi.mock('@/lib/logger', () => ({
  logger: {warn: vi.fn(), error: vi.fn()},
}))

import {useFlowChangeSync, FLOW_CHANGE_DEBOUNCE_MS} from './useFlowChangeSync'

const doc = (name: string) => ({id: 'flow-1', name, subflows: []}) as unknown as FlowDocument

function advance(ms: number) {
  return act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe('useFlowChangeSync', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    presence.listeners = []
    presence.state = {flowId: 'flow-1', status: 'connected'}
    useFlowStore.setState({document: doc('v3'), libraryVersion: 3} as never)
  })
  afterEach(() => vi.useRealTimers())

  it('reloads a version whose download was aborted by a reconnect when the server re-emits it', async () => {
    getContent.mockImplementationOnce(() => new Promise<FlowDocument>(() => {}))
    getContent.mockResolvedValue(doc('v7'))
    get.mockResolvedValue({version: 7})
    const {rerender} = renderHook(() => useFlowChangeSync())

    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(1)

    presence.state = {flowId: 'flow-1', status: 'reconnecting'}
    rerender()
    presence.state = {flowId: 'flow-1', status: 'connected'}
    rerender()

    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(2)
    expect(useFlowStore.getState().document?.name).toBe('v7')
    expect(useFlowStore.getState().libraryVersion).toBe(7)

    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(2)
  })

  it('forgets the applied version when the flow changes', async () => {
    get.mockResolvedValue({version: 7})
    getContent.mockResolvedValue(doc('v7'))
    const {rerender} = renderHook(() => useFlowChangeSync())
    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(1)

    const other = {id: 'flow-2', name: 'other', subflows: []} as unknown as FlowDocument
    useFlowStore.setState({document: other, libraryVersion: 1} as never)
    getContent.mockResolvedValue({...other, name: 'other-v7'})
    presence.state = {flowId: 'flow-2', status: 'connected'}
    rerender()

    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(2)
    expect(getContent).toHaveBeenLastCalledWith('flow-2', expect.anything())
    expect(useFlowStore.getState().document?.name).toBe('other-v7')
  })

  it('coalesces a burst of flow.changed events into ONE reload and applies the last document', async () => {
    get.mockResolvedValue({version: 6})
    getContent.mockResolvedValue(doc('v6'))
    renderHook(() => useFlowChangeSync())

    act(() => {
      presence.emit(4)
      presence.emit(5)
      presence.emit(6)
    })
    expect(getContent).not.toHaveBeenCalled()

    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(1)
    expect(get).toHaveBeenCalledTimes(1)
    expect(useFlowStore.getState().document?.name).toBe('v6')
    expect(useFlowStore.getState().libraryVersion).toBe(6)
  })

  it('aborts a reload still in flight when a later change supersedes it, and ignores its late response', async () => {
    let firstSignal: AbortSignal | undefined
    let resolveFirst!: (d: FlowDocument) => void
    getContent.mockImplementationOnce((_id: string, signal?: AbortSignal) => {
      firstSignal = signal
      return new Promise<FlowDocument>(resolve => {
        resolveFirst = resolve
      })
    })
    getContent.mockResolvedValue(doc('v8'))
    get.mockResolvedValue({version: 8})
    renderHook(() => useFlowChangeSync())

    act(() => presence.emit(7))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(1)
    expect(firstSignal?.aborted).toBe(false)

    act(() => presence.emit(8))
    await advance(FLOW_CHANGE_DEBOUNCE_MS)
    expect(getContent).toHaveBeenCalledTimes(2)
    expect(firstSignal?.aborted).toBe(true)
    expect(useFlowStore.getState().document?.name).toBe('v8')

    await act(async () => {
      resolveFirst(doc('v7-stale'))
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(useFlowStore.getState().document?.name).toBe('v8')
  })

  it('unmounting cancels a pending reload', async () => {
    getContent.mockResolvedValue(doc('v9'))
    get.mockResolvedValue({version: 9})
    const {unmount} = renderHook(() => useFlowChangeSync())
    act(() => presence.emit(9))
    unmount()
    await advance(FLOW_CHANGE_DEBOUNCE_MS * 2)
    expect(getContent).not.toHaveBeenCalled()
    expect(useFlowStore.getState().document?.name).toBe('v3')
  })
})
