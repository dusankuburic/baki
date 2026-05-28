import { describe, it, expect, vi, beforeEach } from 'vitest'
import type { PendingOp } from '@/services/sync/SyncManager'

// vi.mock factories are hoisted before let declarations, so use vi.hoisted() to
// share state between the factory and the test body.
const capture = vi.hoisted(() => ({
  queueChangeHandler: null as ((queue: readonly PendingOp[]) => void) | null,
}))

vi.mock('@/services/sync/SyncManager', () => ({
  syncManager: {
    start: vi.fn(),
    stop: vi.fn(),
    enqueue: vi.fn(),
    getQueue: vi.fn().mockReturnValue([]),
    onQueueChange: vi.fn((h: (q: readonly PendingOp[]) => void) => {
      capture.queueChangeHandler = h
      return () => { capture.queueChangeHandler = null }
    }),
  },
}))

import { useSyncStore } from './syncStore'

const initialState = useSyncStore.getState()

beforeEach(() => {
  useSyncStore.setState(initialState, true)
})

// ---- initial state ----

describe('initial state', () => {
  it('starts with an empty queue', () => {
    expect(useSyncStore.getState().queue).toEqual([])
  })

  it('starts with pendingCount of zero', () => {
    expect(useSyncStore.getState().pendingCount).toBe(0)
  })
})

// ---- queue synchronization ----

describe('queue synchronization', () => {
  it('updates queue and pendingCount when syncManager fires a change', () => {
    const ops: PendingOp[] = [
      { id: 'op-1', env: { type: 'block.update' }, queuedAt: Date.now() },
      { id: 'op-2', env: { type: 'cursor.move' }, queuedAt: Date.now() },
    ]
    capture.queueChangeHandler?.(ops)

    const s = useSyncStore.getState()
    expect(s.queue).toEqual(ops)
    expect(s.pendingCount).toBe(2)
  })

  it('resets to zero when all ops are flushed', () => {
    const op: PendingOp = { id: 'op-1', env: { type: 'block.update' }, queuedAt: Date.now() }
    capture.queueChangeHandler?.([op])
    expect(useSyncStore.getState().pendingCount).toBe(1)

    capture.queueChangeHandler?.([])
    expect(useSyncStore.getState().queue).toEqual([])
    expect(useSyncStore.getState().pendingCount).toBe(0)
  })

  it('the handler was registered with syncManager', () => {
    expect(capture.queueChangeHandler).not.toBeNull()
  })
})
