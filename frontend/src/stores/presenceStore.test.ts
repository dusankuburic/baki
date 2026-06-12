import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('@/services/collaboration/CollaborationService', () => ({
  collaborationService: {
    connect: vi.fn(),
    disconnect: vi.fn(),
    send: vi.fn(),
    getStatus: vi.fn().mockReturnValue('disconnected'),
    subscribe: vi.fn().mockReturnValue(() => {}),
    onStatusChange: vi.fn().mockReturnValue(() => {}),
  },
}))

vi.mock('@/services/sync/SyncManager', () => ({
  syncManager: {
    start: vi.fn(),
    stop: vi.fn(),
    enqueue: vi.fn(),
    getQueue: vi.fn().mockReturnValue([]),
    onQueueChange: vi.fn().mockReturnValue(() => {}),
  },
}))

vi.mock('@/api/client', () => ({
  getBackendConfig: vi.fn().mockResolvedValue({
    apiUrl: 'http://localhost:9999',
    token: 'test-token',
  }),
  getWsTicket: vi.fn().mockResolvedValue('test-ticket'),
}))

import {
  collaborationService,
  type Envelope,
  type ConnectionStatus,
} from '@/services/collaboration/CollaborationService'
import { syncManager } from '@/services/sync/SyncManager'
import { getWsTicket } from '@/api/client'
import { usePresenceStore } from './presenceStore'

const mockConnect = collaborationService.connect as ReturnType<typeof vi.fn>
const mockDisconnect = collaborationService.disconnect as ReturnType<typeof vi.fn>
const mockSubscribe = collaborationService.subscribe as ReturnType<typeof vi.fn>
const mockOnStatusChange = collaborationService.onStatusChange as ReturnType<typeof vi.fn>
const mockSyncStart = syncManager.start as ReturnType<typeof vi.fn>
const mockSyncStop = syncManager.stop as ReturnType<typeof vi.fn>
const mockEnqueue = syncManager.enqueue as ReturnType<typeof vi.fn>

const initialState = usePresenceStore.getState()

beforeEach(() => {
  usePresenceStore.setState(initialState, true)
  vi.clearAllMocks()
  mockSubscribe.mockReturnValue(() => {})
  mockOnStatusChange.mockReturnValue(() => {})
})

// ---- connectToFlow ----

describe('connectToFlow', () => {
  it('connects the collaboration service with backend config', async () => {
    await usePresenceStore.getState().connectToFlow('flow-123')
    // The ticket provider (not the long-lived token) is handed to the service.
    expect(mockConnect).toHaveBeenCalledWith('flow-123', 'http://localhost:9999', getWsTicket)
  })

  it('starts the sync manager', async () => {
    await usePresenceStore.getState().connectToFlow('flow-123')
    expect(mockSyncStart).toHaveBeenCalled()
  })

  it('sets flowId and clears existing users', async () => {
    usePresenceStore.setState({ users: { u1: { userId: 'u1', displayName: 'Alice' } } })
    await usePresenceStore.getState().connectToFlow('flow-abc')

    const s = usePresenceStore.getState()
    expect(s.flowId).toBe('flow-abc')
    expect(s.users).toEqual({})
  })

  it('re-connects when called with a different flow', async () => {
    await usePresenceStore.getState().connectToFlow('flow-1')
    await usePresenceStore.getState().connectToFlow('flow-2')
    expect(mockConnect).toHaveBeenCalledTimes(2)
    expect(usePresenceStore.getState().flowId).toBe('flow-2')
  })
})

// ---- disconnect ----

describe('disconnect', () => {
  it('disconnects the collaboration service', () => {
    usePresenceStore.getState().disconnect()
    expect(mockDisconnect).toHaveBeenCalled()
  })

  it('stops the sync manager', () => {
    usePresenceStore.getState().disconnect()
    expect(mockSyncStop).toHaveBeenCalled()
  })

  it('clears all presence state', () => {
    usePresenceStore.setState({
      users: { u1: { userId: 'u1', displayName: 'Alice' } },
      flowId: 'flow-1',
      status: 'connected' as ConnectionStatus,
    })
    usePresenceStore.getState().disconnect()

    const s = usePresenceStore.getState()
    expect(s.users).toEqual({})
    expect(s.flowId).toBeNull()
    expect(s.status).toBe('disconnected')
  })
})

// ---- updateSelectedBlock ----

describe('updateSelectedBlock', () => {
  it('enqueues a presence.update op with the block id', () => {
    usePresenceStore.getState().updateSelectedBlock('block-42')
    expect(mockEnqueue).toHaveBeenCalledWith({
      type: 'presence.update',
      payload: { selectedBlockId: 'block-42' },
    })
  })
})

// ---- envelope handling ----

describe('envelope handling', () => {
  let handleEnvelope: (env: Envelope) => void

  beforeEach(async () => {
    mockSubscribe.mockImplementation((h: (env: Envelope) => void) => {
      handleEnvelope = h
      return () => {}
    })
    await usePresenceStore.getState().connectToFlow('flow-1')
  })

  it('adds a user on presence.join', () => {
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: { userId: 'u1', displayName: 'Alice' },
    })
    expect(usePresenceStore.getState().users['u1']).toMatchObject({
      userId: 'u1',
      displayName: 'Alice',
    })
  })

  it('falls back to userId as displayName when absent', () => {
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: { userId: 'u2' },
    })
    expect(usePresenceStore.getState().users['u2']?.displayName).toBe('u2')
  })

  it('removes a user on presence.leave', () => {
    usePresenceStore.setState({ users: { u1: { userId: 'u1', displayName: 'Alice' } } })
    handleEnvelope({
      type: 'presence.leave',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
    })
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('ignores presence.leave when userId is missing', () => {
    usePresenceStore.setState({ users: { u1: { userId: 'u1', displayName: 'Alice' } } })
    handleEnvelope({
      type: 'presence.leave',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
    })
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
  })

  it('updates selectedBlockId on presence.update', () => {
    usePresenceStore.setState({ users: { u1: { userId: 'u1', displayName: 'Alice' } } })
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: { selectedBlockId: 'block-5' },
    })
    expect(usePresenceStore.getState().users['u1']?.selectedBlockId).toBe('block-5')
  })

  it('ignores presence.update for an unknown user without crashing', () => {
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'ghost',
      ts: new Date().toISOString(),
      payload: { selectedBlockId: 'block-5' },
    })
    expect(usePresenceStore.getState().users).toEqual({})
  })
})

// ---- status propagation ----

describe('status propagation', () => {
  it('reflects status changes from the collaboration service', async () => {
    let capturedHandler: ((s: ConnectionStatus) => void) | null = null
    mockOnStatusChange.mockImplementation((h: (s: ConnectionStatus) => void) => {
      capturedHandler = h
      return () => {}
    })

    await usePresenceStore.getState().connectToFlow('flow-1')
    ;(capturedHandler as unknown as ((s: ConnectionStatus) => void))?.('connected')

    expect(usePresenceStore.getState().status).toBe('connected')
  })
})
