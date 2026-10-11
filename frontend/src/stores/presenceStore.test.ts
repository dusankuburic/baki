import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

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

vi.mock('@/hooks/usePlatform', () => ({
  notifyIfBackground: vi.fn().mockResolvedValue(undefined),
}))

import {
  collaborationService,
  type Envelope,
  type ConnectionStatus,
  type PresenceSyncPayload,
} from '@/services/collaboration/CollaborationService'
import {syncManager} from '@/services/sync/SyncManager'
import {getWsTicket} from '@/api/client'
import {notifyIfBackground} from '@/hooks/usePlatform'
import {usePresenceStore} from './presenceStore'

const mockConnect = collaborationService.connect as ReturnType<typeof vi.fn>
const mockDisconnect = collaborationService.disconnect as ReturnType<typeof vi.fn>
const mockSubscribe = collaborationService.subscribe as ReturnType<typeof vi.fn>
const mockOnStatusChange = collaborationService.onStatusChange as ReturnType<typeof vi.fn>
const mockSyncStart = syncManager.start as ReturnType<typeof vi.fn>
const mockSyncStop = syncManager.stop as ReturnType<typeof vi.fn>
const mockEnqueue = syncManager.enqueue as ReturnType<typeof vi.fn>
const mockNotify = notifyIfBackground as ReturnType<typeof vi.fn>

const initialState = usePresenceStore.getState()

beforeEach(() => {
  usePresenceStore.setState(initialState, true)
  vi.clearAllMocks()
  mockSubscribe.mockReturnValue(() => {})
  mockOnStatusChange.mockReturnValue(() => {})
})

describe('connectToFlow', () => {
  it('connects the collaboration service with backend config', async () => {
    await usePresenceStore.getState().connectToFlow('flow-123')
    expect(mockConnect).toHaveBeenCalledWith('flow-123', 'http://localhost:9999', getWsTicket)
  })

  it('starts the sync manager', async () => {
    await usePresenceStore.getState().connectToFlow('flow-123')
    expect(mockSyncStart).toHaveBeenCalled()
  })

  it('sets flowId and clears existing users', async () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})
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
      users: {u1: {userId: 'u1', displayName: 'Alice'}},
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

describe('updateSelectedBlock', () => {
  it('enqueues a presence.update op with the block id', () => {
    usePresenceStore.getState().updateSelectedBlock('block-42')
    expect(mockEnqueue).toHaveBeenCalledWith({
      type: 'presence.update',
      payload: {selectedBlockId: 'block-42'},
    })
  })
})

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
      payload: {userId: 'u1', displayName: 'Alice'},
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
      payload: {userId: 'u2'},
    })
    expect(usePresenceStore.getState().users['u2']?.displayName).toBe('u2')
  })

  it('removes a user on presence.leave', () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})
    handleEnvelope({
      type: 'presence.leave',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
    })
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('ignores presence.leave when userId is missing', () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})
    handleEnvelope({
      type: 'presence.leave',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
    })
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
  })

  it('updates selectedBlockId on presence.update', () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {selectedBlockId: 'block-5'},
    })
    expect(usePresenceStore.getState().users['u1']?.selectedBlockId).toBe('block-5')
  })

  it('creates the entry on presence.update from an unknown user', () => {
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u9',
      ts: new Date().toISOString(),
      payload: {userId: 'u9', displayName: 'Nina', selectedBlockId: 'block-5'},
    })
    expect(usePresenceStore.getState().users['u9']).toEqual({
      userId: 'u9',
      displayName: 'Nina',
      selectedBlockId: 'block-5',
    })
  })

  it('falls back to userId as displayName for a user created by presence.update', () => {
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u9',
      ts: new Date().toISOString(),
      payload: {selectedBlockId: 'block-5'},
    })
    expect(usePresenceStore.getState().users['u9']?.displayName).toBe('u9')
  })

  it('takes only selectedBlockId from a presence.update payload', () => {
    usePresenceStore.setState({
      users: {u1: {userId: 'u1', displayName: 'Alice', avatarUrl: 'https://cdn.example/alice.png'}},
    })
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {
        userId: 'admin',
        displayName: 'Administrator',
        avatarUrl: 'https://evil.example/x.png',
        selectedBlockId: 'block-7',
      },
    })
    const users = usePresenceStore.getState().users
    expect(users['u1']).toEqual({
      userId: 'u1',
      displayName: 'Alice',
      avatarUrl: 'https://cdn.example/alice.png',
      selectedBlockId: 'block-7',
    })
    expect(users['admin']).toBeUndefined()
  })

  it('clears selectedBlockId when a presence.update carries none', () => {
    for (const payload of [
      {userId: 'u1', displayName: 'Alice', selectedBlockId: ''},
      {userId: 'u1', displayName: 'Alice'},
    ]) {
      usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice', selectedBlockId: 'block-5'}}})
      handleEnvelope({type: 'presence.update', flowId: 'flow-1', userId: 'u1', ts: new Date().toISOString(), payload})
      expect(usePresenceStore.getState().users['u1']?.selectedBlockId).toBeUndefined()
    }
  })

  it('ignores presence.update when userId is missing', () => {
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', selectedBlockId: 'block-5'},
    })
    expect(usePresenceStore.getState().users).toEqual({})
  })

  it('notifies for a genuinely new presence.join only', () => {
    const join: Envelope = {
      type: 'presence.join',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice'},
    }
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      userId: 'me',
      ts: new Date().toISOString(),
      payload: {users: [{userId: 'me', displayName: 'Me'}]},
    })
    handleEnvelope(join)
    handleEnvelope(join)
    expect(mockNotify).toHaveBeenCalledTimes(1)
  })

  it('merges a presence.join for a user already listed', () => {
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {
        users: [
          {
            userId: 'u1',
            displayName: 'Alice',
            avatarUrl: 'https://cdn.example/alice.png',
            selectedBlockId: 'block-3',
          },
        ],
      } satisfies PresenceSyncPayload,
    })
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice Smith'},
    })
    expect(usePresenceStore.getState().users['u1']).toEqual({
      userId: 'u1',
      displayName: 'Alice Smith',
      avatarUrl: 'https://cdn.example/alice.png',
      selectedBlockId: 'block-3',
    })

    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {
        userId: 'u1',
        displayName: 'Alice Smith',
        avatarUrl: 'https://cdn.example/alice-2.png',
        selectedBlockId: 'block-4',
      },
    })
    expect(usePresenceStore.getState().users['u1']).toEqual({
      userId: 'u1',
      displayName: 'Alice Smith',
      avatarUrl: 'https://cdn.example/alice-2.png',
      selectedBlockId: 'block-4',
    })
    expect(mockNotify).not.toHaveBeenCalled()
  })

  it('leaves the users map untouched on a presence.join that changes nothing', () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice', selectedBlockId: 'block-3'}}})
    const before = usePresenceStore.getState().users
    const listener = vi.fn()
    const unsubscribe = usePresenceStore.subscribe(listener)
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice'},
    })
    expect(usePresenceStore.getState().users).toBe(before)
    expect(listener).not.toHaveBeenCalled()
    unsubscribe()
  })

  it('replaces the users map on presence.sync without notifying', () => {
    usePresenceStore.setState({users: {ghost: {userId: 'ghost', displayName: 'Left while we were away'}}})
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {
        users: [
          {userId: 'me', displayName: 'Me'},
          {userId: 'u1', displayName: 'Alice', selectedBlockId: 'block-3'},
          {userId: 'u2'},
        ],
      } satisfies PresenceSyncPayload,
    })
    const users = usePresenceStore.getState().users
    expect(Object.keys(users).sort()).toEqual(['me', 'u1', 'u2'])
    expect(users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice', selectedBlockId: 'block-3'})
    expect(users['u2']?.displayName).toBe('u2')
    expect(mockNotify).not.toHaveBeenCalled()
  })

  it('never shows a selection on the local user entry', () => {
    const ts = new Date().toISOString()
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      userId: 'me',
      ts,
      payload: {
        users: [
          {userId: 'me', displayName: 'Me', selectedBlockId: 'block-1'},
          {userId: 'u2', displayName: 'Bob'},
        ],
      } satisfies PresenceSyncPayload,
    })
    expect(usePresenceStore.getState().users['me']?.selectedBlockId).toBeUndefined()

    const before = usePresenceStore.getState().users
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'me',
      ts,
      payload: {userId: 'me', displayName: 'Me', selectedBlockId: 'block-1'},
    })
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'me',
      ts,
      payload: {userId: 'me', displayName: 'Me', selectedBlockId: 'block-2'},
    })
    expect(usePresenceStore.getState().users['me']?.selectedBlockId).toBeUndefined()
    expect(usePresenceStore.getState().users).toBe(before)

    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u2',
      ts,
      payload: {userId: 'u2', displayName: 'Bob', selectedBlockId: 'block-1'},
    })
    expect(usePresenceStore.getState().users['u2']?.selectedBlockId).toBe('block-1')
  })

  it('ignores a malformed presence.sync payload', () => {
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})
    for (const payload of [undefined, null, 'nope', {}, {users: 'nope'}]) {
      handleEnvelope({type: 'presence.sync', flowId: 'flow-1', ts: new Date().toISOString(), payload})
    }
    expect(usePresenceStore.getState().users).toEqual({u1: {userId: 'u1', displayName: 'Alice'}})
  })

  it('skips presence.sync entries without a userId', () => {
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {users: [null, 42, {displayName: 'No id'}, {userId: ''}, {userId: 'u1'}]},
    })
    expect(Object.keys(usePresenceStore.getState().users)).toEqual(['u1'])
  })

  it('removes a user seeded by presence.sync on presence.leave', () => {
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {users: [{userId: 'u1'}, {userId: 'u2'}]} satisfies PresenceSyncPayload,
    })
    handleEnvelope({
      type: 'presence.leave',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
    })
    expect(Object.keys(usePresenceStore.getState().users)).toEqual(['u2'])
  })
})

describe('presence sweep timer', () => {
  beforeEach(() => {
    usePresenceStore.getState().disconnect()
    vi.useFakeTimers()
  })
  afterEach(() => {
    usePresenceStore.getState().disconnect()
    vi.useRealTimers()
  })

  it('sweeps stale users while connected', async () => {
    await usePresenceStore.getState().connectToFlow('flow-1')
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})

    vi.advanceTimersByTime(60_000)
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('clears the interval and stops sweeping after disconnect', async () => {
    const clearSpy = vi.spyOn(globalThis, 'clearInterval')
    await usePresenceStore.getState().connectToFlow('flow-1')

    usePresenceStore.getState().disconnect()
    expect(clearSpy).toHaveBeenCalled()

    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}, flowId: 'flow-1'})
    vi.advanceTimersByTime(120_000)
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
  })

  it('does not start a second interval when already connected', async () => {
    const setSpy = vi.spyOn(globalThis, 'setInterval')
    await usePresenceStore.getState().connectToFlow('flow-1')
    await usePresenceStore.getState().connectToFlow('flow-2')
    const sweepStarts = setSpy.mock.calls.filter(([, ms]) => ms === 60_000)
    expect(sweepStarts.length).toBe(1)
  })
})

describe('presence heartbeat vs. the sweep', () => {
  const HEARTBEAT_MS = 54_000
  let handleEnvelope: (env: Envelope) => void

  const heartbeat = (userId: string) =>
    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId,
      ts: new Date().toISOString(),
      payload: {userId, displayName: 'Alice', selectedBlockId: ''},
    })

  beforeEach(async () => {
    usePresenceStore.getState().disconnect()
    vi.useFakeTimers()
    mockSubscribe.mockImplementation((h: (env: Envelope) => void) => {
      handleEnvelope = h
      return () => {}
    })
    await usePresenceStore.getState().connectToFlow('flow-1')
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
      payload: {users: [{userId: 'u1', displayName: 'Alice'}]} satisfies PresenceSyncPayload,
    })
  })
  afterEach(() => {
    usePresenceStore.getState().disconnect()
    vi.useRealTimers()
  })

  it('keeps an idle user present across five minutes of heartbeats', () => {
    for (let i = 0; i < 5; i++) {
      vi.advanceTimersByTime(HEARTBEAT_MS)
      heartbeat('u1')
    }
    vi.advanceTimersByTime(300_000 - 5 * HEARTBEAT_MS)
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  it('sweeps a user with no heartbeat after the stale window plus one sweep interval', () => {
    vi.advanceTimersByTime(120_000)
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
    vi.advanceTimersByTime(60_000)
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('restores a swept user on its next heartbeat', () => {
    vi.advanceTimersByTime(180_000)
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()

    heartbeat('u1')
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  it('leaves the users map untouched on heartbeats that change nothing', () => {
    const before = usePresenceStore.getState().users
    const listener = vi.fn()
    const unsubscribe = usePresenceStore.subscribe(listener)
    heartbeat('u1')
    heartbeat('u1')
    expect(usePresenceStore.getState().users).toBe(before)
    expect(listener).not.toHaveBeenCalled()

    handleEnvelope({
      type: 'presence.update',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice', selectedBlockId: 'block-2'},
    })
    expect(listener).toHaveBeenCalledTimes(1)
    expect(usePresenceStore.getState().users['u1']?.selectedBlockId).toBe('block-2')
    unsubscribe()
  })

  it('counts a presence.join for a listed user as seen', () => {
    vi.advanceTimersByTime(100_000)
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice'},
    })
    vi.advanceTimersByTime(80_000)
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  it('never sweeps the local user, whom no heartbeat refreshes', () => {
    handleEnvelope({
      type: 'presence.sync',
      flowId: 'flow-1',
      userId: 'me',
      ts: new Date().toISOString(),
      payload: {
        users: [
          {userId: 'me', displayName: 'Me'},
          {userId: 'u1', displayName: 'Alice'},
        ],
      } satisfies PresenceSyncPayload,
    })
    vi.advanceTimersByTime(180_000)
    const users = usePresenceStore.getState().users
    expect(users['me']).toBeDefined()
    expect(users['u1']).toBeUndefined()
  })
})

describe('join notifications across reconnects', () => {
  let handleEnvelope: (env: Envelope) => void
  let setStatus: (s: ConnectionStatus) => void
  const ts = () => new Date().toISOString()
  const join = (userId: string, displayName: string): Envelope => ({
    type: 'presence.join',
    flowId: 'flow-1',
    userId,
    ts: ts(),
    payload: {userId, displayName},
  })
  const sync = (...users: Array<{userId: string; displayName: string}>): Envelope => ({
    type: 'presence.sync',
    flowId: 'flow-1',
    userId: 'me',
    ts: ts(),
    payload: {users},
  })

  beforeEach(async () => {
    mockSubscribe.mockImplementation((h: (env: Envelope) => void) => {
      handleEnvelope = h
      return () => {}
    })
    mockOnStatusChange.mockImplementation((h: (s: ConnectionStatus) => void) => {
      setStatus = h
      return () => {}
    })
    await usePresenceStore.getState().connectToFlow('flow-1')
  })

  it('does not announce the local user when its own join precedes the sync', () => {
    setStatus('connected')
    handleEnvelope(join('me', 'Me'))
    handleEnvelope(sync({userId: 'me', displayName: 'Me'}))
    expect(mockNotify).not.toHaveBeenCalled()

    setStatus('error')
    setStatus('connecting')
    setStatus('connected')
    expect(usePresenceStore.getState().users).toEqual({})
    handleEnvelope(join('me', 'Me'))
    expect(mockNotify).not.toHaveBeenCalled()
    handleEnvelope(sync({userId: 'me', displayName: 'Me'}, {userId: 'u1', displayName: 'Alice'}))
    expect(mockNotify).not.toHaveBeenCalled()
  })

  it('still announces a collaborator who joins after the sync', () => {
    setStatus('connected')
    handleEnvelope(join('me', 'Me'))
    handleEnvelope(sync({userId: 'me', displayName: 'Me'}))
    handleEnvelope(join('u1', 'Alice'))
    expect(mockNotify).toHaveBeenCalledTimes(1)
    expect(mockNotify).toHaveBeenCalledWith({title: 'Collaborator joined', body: 'Alice joined the flow.'})

    setStatus('connecting')
    setStatus('connected')
    handleEnvelope(join('me', 'Me'))
    handleEnvelope(sync({userId: 'me', displayName: 'Me'}, {userId: 'u1', displayName: 'Alice'}))
    handleEnvelope(join('u2', 'Bob'))
    expect(mockNotify).toHaveBeenCalledTimes(2)
  })
})

describe('status propagation', () => {
  it('reflects status changes from the collaboration service', async () => {
    let capturedHandler: ((s: ConnectionStatus) => void) | null = null
    mockOnStatusChange.mockImplementation((h: (s: ConnectionStatus) => void) => {
      capturedHandler = h
      return () => {}
    })

    await usePresenceStore.getState().connectToFlow('flow-1')
    ;(capturedHandler as unknown as (s: ConnectionStatus) => void)?.('connected')

    expect(usePresenceStore.getState().status).toBe('connected')
  })
})
