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

// ---- updateSelectedBlock ----

describe('updateSelectedBlock', () => {
  it('enqueues a presence.update op with the block id', () => {
    usePresenceStore.getState().updateSelectedBlock('block-42')
    expect(mockEnqueue).toHaveBeenCalledWith({
      type: 'presence.update',
      payload: {selectedBlockId: 'block-42'},
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

  // Regression: presence.update from a user with no entry was dropped, so a
  // peer missed at join (or swept while idle) never reappeared. The server's
  // presence heartbeat relies on an update creating the entry.
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

  // Regression: the raw payload was merged into the entry ({...existing,
  // ...p}), so a peer could rename itself, swap its avatar or re-key its
  // entry as another user in everyone else's presence list.
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

  // The payload is the user's whole current selection — the server sends ''
  // for none; an older server omitted the key — so either clears it. The old
  // merge kept the previous block, leaving a stale "viewing block" ring behind.
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

  // Regression: when a user already in the room opened a second tab, the
  // server's presence.join for it (userId and displayName only) replaced the
  // whole entry, so every peer lost that user's selection ring — and avatar —
  // until the next heartbeat, up to a ping period (54 s) later.
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

    // What the payload does carry still wins.
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

  // Regression: a newly joined client never learned who was already in the
  // room — only peers who joined after it. presence.sync is the server's
  // snapshot, sent to the joiner only; it is authoritative, so it replaces
  // the map (dropping ghosts), and these users were here first, so no
  // "collaborator joined" notification fires for them.
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

  // Regression: the server announces a user's selection to that user's other
  // connections too, so with a second tab open its heartbeat carried the
  // block just selected in this tab back here. The local user's entry took
  // it, and the user's own avatar sat on their selection as a teammate's —
  // on the block row, as the graph's teammate border, as the ring.
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

    // A collaborator's selection still lands.
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

// ---- presence sweep timer (lifecycle-managed, no module-level leak) ----

describe('presence sweep timer', () => {
  beforeEach(() => {
    // Clear any sweep a prior test left running, then switch to fake timers.
    usePresenceStore.getState().disconnect()
    vi.useFakeTimers()
  })
  afterEach(() => {
    usePresenceStore.getState().disconnect()
    vi.useRealTimers()
  })

  it('sweeps stale users while connected', async () => {
    await usePresenceStore.getState().connectToFlow('flow-1')
    // A user with no lastSeen is treated as stale immediately.
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}})

    vi.advanceTimersByTime(60_000) // one sweep tick
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('clears the interval and stops sweeping after disconnect', async () => {
    const clearSpy = vi.spyOn(globalThis, 'clearInterval')
    await usePresenceStore.getState().connectToFlow('flow-1')

    usePresenceStore.getState().disconnect()
    expect(clearSpy).toHaveBeenCalled()

    // With the timer stopped, a later stale user is NOT swept.
    usePresenceStore.setState({users: {u1: {userId: 'u1', displayName: 'Alice'}}, flowId: 'flow-1'})
    vi.advanceTimersByTime(120_000)
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
  })

  it('does not start a second interval when already connected', async () => {
    const setSpy = vi.spyOn(globalThis, 'setInterval')
    await usePresenceStore.getState().connectToFlow('flow-1')
    await usePresenceStore.getState().connectToFlow('flow-2')
    // startPresenceSweep is guarded, so only one interval is ever created.
    const sweepStarts = setSpy.mock.calls.filter(([, ms]) => ms === 60_000)
    expect(sweepStarts.length).toBe(1)
  })
})

// ---- presence heartbeat vs. the sweep ----
//
// Regression: an idle collaborator (no selection changes) sent nothing, so the
// 120 s sweep dropped them and, with updates from unknown users ignored, they
// never came back. The server now re-broadcasts each live connection's
// presence.update every ping period as a heartbeat.
describe('presence heartbeat vs. the sweep', () => {
  // Must mirror pingPeriod in internal/websocket/client.go.
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
    // Known only through the join snapshot, like any user already in the room
    // when we connected.
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
    // Heartbeats at 54, 108, 162, 216 and 270 s; sweeps every 60 s to 300 s.
    for (let i = 0; i < 5; i++) {
      vi.advanceTimersByTime(HEARTBEAT_MS)
      heartbeat('u1')
    }
    vi.advanceTimersByTime(300_000 - 5 * HEARTBEAT_MS)
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  it('sweeps a user with no heartbeat after the stale window plus one sweep interval', () => {
    vi.advanceTimersByTime(120_000) // sweep at exactly the 120 s window: kept
    expect(usePresenceStore.getState().users['u1']).toBeDefined()
    vi.advanceTimersByTime(60_000) // next sweep at 180 s: stale
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()
  })

  it('restores a swept user on its next heartbeat', () => {
    vi.advanceTimersByTime(180_000)
    expect(usePresenceStore.getState().users['u1']).toBeUndefined()

    heartbeat('u1')
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  // Regression: every heartbeat replaced the users map (its lastSeen field
  // changed), so each one — one per peer connection per ping period —
  // re-rendered every visible block row and walked every graph node although
  // nothing on screen had changed.
  it('leaves the users map untouched on heartbeats that change nothing', () => {
    const before = usePresenceStore.getState().users
    const listener = vi.fn()
    const unsubscribe = usePresenceStore.subscribe(listener)
    heartbeat('u1')
    heartbeat('u1')
    expect(usePresenceStore.getState().users).toBe(before)
    expect(listener).not.toHaveBeenCalled()

    // A real change still lands.
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

  // A merged presence.join (a listed user's second tab) changes nothing on
  // screen, but it is still word from that user and must count as seen.
  it('counts a presence.join for a listed user as seen', () => {
    vi.advanceTimersByTime(100_000)
    handleEnvelope({
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice'},
    })
    vi.advanceTimersByTime(80_000) // sweeps at 120 s and 180 s: seen 80 s ago
    expect(usePresenceStore.getState().users['u1']).toMatchObject({userId: 'u1', displayName: 'Alice'})
  })

  // Regression: the server never sends a connection its own heartbeat, so the
  // local user's entry was never refreshed and the sweep dropped it two to
  // three minutes after joining — with the whole indicator, status dot
  // included, for a user alone in the flow.
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

// ---- join notifications across reconnects ----

// Regression: the server answers every (re)connect with the connection's own
// presence.join before its presence.sync. After a socket error the map is
// already empty, so that join read as a new arrival and a reconnect in a
// hidden tab told the user "Me joined the flow".
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

    // After an error the map is cleared; the new socket's own join again
    // arrives first and must stay quiet.
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

    // And again once a reconnect has synced.
    setStatus('connecting')
    setStatus('connected')
    handleEnvelope(join('me', 'Me'))
    handleEnvelope(sync({userId: 'me', displayName: 'Me'}, {userId: 'u1', displayName: 'Alice'}))
    handleEnvelope(join('u2', 'Bob'))
    expect(mockNotify).toHaveBeenCalledTimes(2)
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
    ;(capturedHandler as unknown as (s: ConnectionStatus) => void)?.('connected')

    expect(usePresenceStore.getState().status).toBe('connected')
  })
})
