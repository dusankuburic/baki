import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

const mockStatus = {
  value: 'disconnected' as string,
  cb: null as ((s: string) => void) | null,
}
const sentEnvs: unknown[] = []

vi.mock('@/services/collaboration/CollaborationService', () => ({
  collaborationService: {
    getStatus: () => mockStatus.value,
    send: (env: unknown) => { sentEnvs.push(env) },
    onStatusChange: (cb: (s: string) => void) => {
      mockStatus.cb = cb
      return () => { mockStatus.cb = null }
    },
  },
}))

import { syncManager } from './SyncManager'

beforeEach(() => {
  localStorage.clear()
  sentEnvs.length = 0
  mockStatus.value = 'disconnected'
  syncManager.stop()
})

afterEach(() => {
  syncManager.stop()
  localStorage.clear()
})

describe('start(flowId)', () => {
  it('loads persisted queue from localStorage', () => {
    const flowId = 'flow-1'
    const op = { id: 'op-1-1', env: { type: 'block.update' }, queuedAt: Date.now() }
    localStorage.setItem(`baki-sync-queue-${flowId}`, JSON.stringify({ queue: [op], counter: 1 }))

    syncManager.start(flowId)

    expect(syncManager.getQueue()).toHaveLength(1)
    expect(syncManager.getQueue()[0].id).toBe('op-1-1')
  })

  it('drops stale ops on load (TTL expired)', () => {
    const flowId = 'flow-2'
    const freshOp = { id: 'op-1', env: { type: 'block.update' }, queuedAt: Date.now() }
    const staleOp = { id: 'op-2', env: { type: 'cursor.move' }, queuedAt: Date.now() - 120_000 }
    localStorage.setItem(`baki-sync-queue-${flowId}`, JSON.stringify({ queue: [freshOp, staleOp], counter: 2 }))

    syncManager.start(flowId)

    expect(syncManager.getQueue()).toHaveLength(1)
    expect(syncManager.getQueue()[0].id).toBe('op-1')
  })

  it('starts empty when no persisted data exists', () => {
    syncManager.start('flow-3')
    expect(syncManager.getQueue()).toHaveLength(0)
  })

  it('handles corrupt localStorage data gracefully', () => {
    localStorage.setItem('baki-sync-queue-flow-4', '{not valid json')
    syncManager.start('flow-4')
    expect(syncManager.getQueue()).toHaveLength(0)
  })

  it('switches queues when flowId changes', () => {
    localStorage.setItem('baki-sync-queue-a', JSON.stringify({
      queue: [{ id: 'a1', env: { type: 'block.update' }, queuedAt: Date.now() }],
      counter: 1,
    }))
    localStorage.setItem('baki-sync-queue-b', JSON.stringify({
      queue: [{ id: 'b1', env: { type: 'cursor.move' }, queuedAt: Date.now() }],
      counter: 1,
    }))

    syncManager.start('a')
    expect(syncManager.getQueue()).toHaveLength(1)
    expect(syncManager.getQueue()[0].id).toBe('a1')

    syncManager.start('b')
    expect(syncManager.getQueue()).toHaveLength(1)
    expect(syncManager.getQueue()[0].id).toBe('b1')
  })
})

describe('enqueue()', () => {
  it('persists to localStorage when offline', () => {
    mockStatus.value = 'disconnected'
    syncManager.start('flow-enq-1')

    syncManager.enqueue({ type: 'block.update', payload: { blockId: 'b1' } })

    const raw = localStorage.getItem('baki-sync-queue-flow-enq-1')
    expect(raw).not.toBeNull()
    const data = JSON.parse(raw!)
    expect(data.queue).toHaveLength(1)
    expect(data.queue[0].env.type).toBe('block.update')
  })

  it('sends immediately when connected (no persistence)', () => {
    mockStatus.value = 'connected'
    syncManager.start('flow-enq-2')

    syncManager.enqueue({ type: 'block.update' })

    expect(sentEnvs).toHaveLength(1)
    expect(syncManager.getQueue()).toHaveLength(0)
    expect(localStorage.getItem('baki-sync-queue-flow-enq-2')).toBeNull()
  })

  it('trims queue at MAX_QUEUE_SIZE', () => {
    mockStatus.value = 'disconnected'
    syncManager.start('flow-enq-3')

    for (let i = 0; i < 210; i++) {
      syncManager.enqueue({ type: 'block.update' })
    }

    expect(syncManager.getQueue().length).toBe(200)
  })
})

describe('flush on reconnect', () => {
  it('sends queued ops and clears storage', () => {
    const flowId = 'flow-flush-1'
    syncManager.start(flowId)
    syncManager.enqueue({ type: 'block.update' })
    syncManager.enqueue({ type: 'cursor.move' })

    expect(localStorage.getItem(`baki-sync-queue-${flowId}`)).not.toBeNull()

    mockStatus.value = 'connected'
    mockStatus.cb?.('connected')

    expect(sentEnvs).toHaveLength(2)
    expect(syncManager.getQueue()).toHaveLength(0)
    expect(localStorage.getItem(`baki-sync-queue-${flowId}`)).toBeNull()
  })
})

describe('stop()', () => {
  it('persists queue to localStorage', () => {
    syncManager.start('flow-stop-1')
    syncManager.enqueue({ type: 'block.update' })

    syncManager.stop()

    const raw = localStorage.getItem('baki-sync-queue-flow-stop-1')
    expect(raw).not.toBeNull()
    const data = JSON.parse(raw!)
    expect(data.queue).toHaveLength(1)
  })
})
