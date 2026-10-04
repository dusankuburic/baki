import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {
  collaborationService,
  type ConnectionStatus,
  type PresencePayload,
  type PresenceSyncPayload,
  type FlowChangedPayload,
  type Envelope,
  type ErrorPayload,
} from '@/services/collaboration/CollaborationService'
import {syncManager} from '@/services/sync/SyncManager'
import {getBackendConfig, getWsTicket} from '@/api/client'
import {notifyIfBackground} from '@/hooks/usePlatform'

export interface PresenceUser {
  userId: string
  displayName: string
  avatarUrl?: string
  selectedBlockId?: string
}

// WsError is the last server `error` envelope (permission_denied,
// unknown_event, …). PresenceIndicators surfaces it as a toast; before it was
// dropped on the floor, so a read-only collaborator whose edit was refused
// over the socket saw nothing happen.
export interface WsError {
  code: string
  message: string
  at: number
}

interface PresenceState {
  users: Record<string, PresenceUser>
  status: ConnectionStatus
  flowId: string | null
  remoteVersion: number
  lastError: WsError | null

  connectToFlow: (flowId: string) => Promise<void>
  disconnect: () => void
  updateSelectedBlock: (blockId: string) => void
}

let flowChangeListeners: Array<(version: number) => void> = []
let cleanupHandlers: Array<() => void> = []
let generation = 0

// lastSeenByUser is when each listed user was last heard from, read only by
// the stale-entry sweep. It lives outside the store on purpose: as a field of
// each entry it changed on every server heartbeat, so every heartbeat — one
// per peer connection per ping period — replaced the users map, re-rendering
// every visible block row and walking every graph node while nothing on
// screen had changed.
const lastSeenByUser = new Map<string, number>()

// selfUserId is the local user, as the server names it on our presence.sync.
// The sweep must never drop it: the server does not send a connection its own
// heartbeat, so nothing refreshes the local user's entry after the join, and
// it — with the whole indicator, for a user alone in the flow — was swept
// two to three minutes after every (re)connect. Its entry also never shows a
// selection (peerSelection).
let selfUserId: string | null = null

// synced: this connection's presence.sync has arrived. The server answers
// every (re)connect with the connection's OWN presence.join first, and after a
// socket error the map is empty by then, so that join looked like a new
// arrival: a reconnect while the tab was hidden (sleep/resume, a network blip)
// raised "Collaborator joined" for the local user. Joins before the sync raise
// no notification; the snapshot covers whoever they announced.
let synced = false

export function onFlowChanged(cb: (version: number) => void): () => void {
  flowChangeListeners.push(cb)
  return () => {
    flowChangeListeners = flowChangeListeners.filter(fn => fn !== cb)
  }
}

function teardown(): void {
  generation++
  cleanupHandlers.forEach(fn => fn())
  cleanupHandlers = []
  lastSeenByUser.clear()
  selfUserId = null
  synced = false
  // Clear flow-change listeners too: on logout the only subscriber
  // (useFlowChangeSync) unmounts and unsubs itself, but clearing here is the
  // symmetric, defensive choice — a subscriber that survives logout (e.g. a
  // portal outside ProtectedRoute) would otherwise accumulate stale listeners
  // across sessions on a shared device.
  flowChangeListeners = []
}

export const usePresenceStore = create<PresenceState>(set => ({
  users: {},
  status: 'disconnected',
  flowId: null,
  remoteVersion: 0,
  lastError: null,

  connectToFlow: async (flowId: string) => {
    teardown()

    const gen = generation
    const cfg = await getBackendConfig()

    if (gen !== generation) return

    set({flowId, users: {}, remoteVersion: 0})

    collaborationService.connect(flowId, cfg.apiUrl, getWsTicket)
    syncManager.start(flowId)
    startPresenceSweep()

    cleanupHandlers.push(
      collaborationService.onStatusChange(status => {
        set({status})
        // Any status but 'connected' means the next socket starts over with
        // its own join and sync.
        if (status !== 'connected') synced = false
        // Clear stale users on disconnect/error so the UI doesn't
        // show ghosts from before the connection dropped. The list
        // repopulates on the next (re)connect without waiting for anyone to
        // re-join: the server answers our own presence.join with a
        // presence.sync snapshot of everyone already in the room, and every
        // live connection's presence.update heartbeat (each server ping
        // period, 54 s) re-adds a peer the snapshot missed.
        if (status === 'disconnected' || status === 'error') {
          lastSeenByUser.clear()
          set({users: {}})
        }
      }),
      collaborationService.subscribe(handleEnvelope),
    )
  },

  disconnect: () => {
    teardown()
    stopPresenceSweep()
    collaborationService.disconnect()
    syncManager.stop()
    set({users: {}, status: 'disconnected', flowId: null, remoteVersion: 0})
  },

  updateSelectedBlock: (blockId: string) => {
    syncManager.enqueue({
      type: 'presence.update',
      payload: {selectedBlockId: blockId} satisfies Partial<PresencePayload>,
    })
  },
}))

// nonEmptyString narrows one untrusted payload field: anything but a
// non-empty string (missing, '', null, a number from a malformed frame)
// reads as absent.
function nonEmptyString(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined
}

// peerSelection is the selection a user's entry may show: none for the local
// user. Every view draws a listed user's selection as a collaborator's — the
// avatar on the block row, the graph's teammate border, the "viewing block"
// ring — and the server announces a user's selection to that user's other
// connections too: with a second tab open, its heartbeat carried the block
// just selected in this one back here, where it showed as a teammate on the
// user's own selection. This tab's selection has its own, local marker.
function peerSelection(userId: string, sel: unknown): string | undefined {
  return userId === selfUserId ? undefined : nonEmptyString(sel)
}

function handleEnvelope(env: Envelope): void {
  if (env.type === 'error') {
    const p = (env.payload ?? {}) as Partial<ErrorPayload>
    usePresenceStore.setState({
      lastError: {
        code: p.code ?? 'error',
        message: p.message ?? 'The collaboration server rejected a message.',
        at: Date.now(),
      },
    })
    return
  }
  if (env.type === 'presence.join') {
    const p = env.payload as PresencePayload
    const wasAlreadyPresent = Boolean(usePresenceStore.getState().users[p.userId])
    lastSeenByUser.set(p.userId, Date.now())
    usePresenceStore.setState(s => {
      const existing = s.users[p.userId]
      if (!existing) {
        return {
          users: {
            ...s.users,
            [p.userId]: {
              userId: p.userId,
              displayName: p.displayName ?? p.userId,
              avatarUrl: p.avatarUrl,
              selectedBlockId: peerSelection(p.userId, p.selectedBlockId),
            },
          },
        }
      }
      // A join for a user already listed is that user opening another tab (or
      // a reconnect whose new socket joined before the old one closed). The
      // server's join payload carries only userId and displayName — the
      // selection is kept per user and reaches peers with the next heartbeat —
      // so replacing the entry wiped the user's "viewing block" ring on every
      // peer for up to a ping period (54 s). Take only what the payload
      // carries; an entry that ends up unchanged keeps the map, like a
      // heartbeat, so no subscriber re-renders.
      const displayName = nonEmptyString(p.displayName) ?? existing.displayName
      const avatarUrl = nonEmptyString(p.avatarUrl) ?? existing.avatarUrl
      const selectedBlockId =
        p.userId === selfUserId ? undefined : (nonEmptyString(p.selectedBlockId) ?? existing.selectedBlockId)
      if (
        displayName === existing.displayName &&
        avatarUrl === existing.avatarUrl &&
        selectedBlockId === existing.selectedBlockId
      ) {
        return s
      }
      return {users: {...s.users, [p.userId]: {...existing, displayName, avatarUrl, selectedBlockId}}}
    })
    // Notify the user that a collaborator joined — but only for a genuinely
    // new arrival (not a reconnect re-announce, nor our own join ahead of the
    // sync) and only when the app is in the background, so it never
    // interrupts an active session.
    if (!wasAlreadyPresent && synced) {
      const name = p.displayName ?? p.userId
      void notifyIfBackground({title: 'Collaborator joined', body: `${name} joined the flow.`})
    }
  } else if (env.type === 'presence.leave') {
    if (!env.userId) return
    lastSeenByUser.delete(env.userId)
    usePresenceStore.setState(s => {
      const users = {...s.users}
      delete users[env.userId!]
      return {users}
    })
  } else if (env.type === 'presence.sync') {
    // The server's snapshot of everyone already in the room, sent to this
    // connection only, right after its own presence.join. Before it existed a
    // newly joined client never learned who was already present. It REPLACES
    // the map — it is the authoritative view, so it also drops ghosts left
    // over from before a silent reconnect — and it raises no "collaborator
    // joined" notifications: these users were here first.
    const p = env.payload as Partial<PresenceSyncPayload> | null | undefined
    if (!p || !Array.isArray(p.users)) return
    synced = true
    // The server addresses the sync to us, so its userId is the local user.
    selfUserId = nonEmptyString(env.userId) ?? null
    const now = Date.now()
    const users: Record<string, PresenceUser> = {}
    lastSeenByUser.clear()
    for (const entry of p.users as Array<Partial<PresencePayload> | null | undefined>) {
      const userId = nonEmptyString(entry?.userId)
      if (!userId) continue
      users[userId] = {
        userId,
        displayName: nonEmptyString(entry?.displayName) ?? userId,
        avatarUrl: nonEmptyString(entry?.avatarUrl),
        selectedBlockId: peerSelection(userId, entry?.selectedBlockId),
      }
      lastSeenByUser.set(userId, now)
    }
    usePresenceStore.setState({users})
  } else if (env.type === 'presence.update') {
    // Keyed by the server-stamped env.userId, and only selectedBlockId is
    // read from the payload. It used to be merged wholesale ({...existing,
    // ...p}), so a peer could rewrite its own displayName/avatarUrl/userId as
    // every other collaborator saw it. The server now builds this payload
    // itself and re-broadcasts it every ping period as a presence heartbeat,
    // so an unknown user is CREATED rather than ignored — that is how a peer
    // missed at join, or swept while idle, comes back. displayName is taken
    // only for that new entry; an existing user is never renamed by it.
    if (!env.userId) return
    const userId = env.userId
    const p = (env.payload ?? {}) as Partial<PresencePayload>
    // The payload carries the user's whole current selection, not a patch —
    // the server sends '' for none — so an absent/empty selectedBlockId means
    // "nothing selected" and must clear the old one rather than keep it. The
    // server picks it per user, not per connection, so the heartbeat of a
    // user's idle second tab carries the block their other tab has selected.
    const selectedBlockId = peerSelection(userId, p.selectedBlockId)
    lastSeenByUser.set(userId, Date.now())
    usePresenceStore.setState(s => {
      const existing = s.users[userId]
      // Most updates are heartbeats that change nothing on screen: keep the
      // map (returning the same state is a no-op) so no subscriber re-renders.
      if (existing && existing.selectedBlockId === selectedBlockId) return s
      return {
        users: {
          ...s.users,
          [userId]: existing
            ? {...existing, selectedBlockId}
            : {userId, displayName: nonEmptyString(p.displayName) ?? userId, selectedBlockId},
        },
      }
    })
  } else if (env.type === 'flow.changed') {
    const p = env.payload as FlowChangedPayload
    usePresenceStore.setState({remoteVersion: p.version})
    flowChangeListeners.forEach(fn => fn(p.version))
  }
}

// Reset on logout (see storeRegistry). disconnect() re-persists the sync queue
// via syncManager.stop(); syncManager.reset() then discards it — order matters,
// so keep these two sequential here rather than as separate handlers.
registerStoreReset(() => {
  usePresenceStore.getState().disconnect()
  syncManager.reset()
})

// Periodic sweep of stale presence entries. A lost presence.leave event
// (server-side drop, brief restart) would otherwise leave a "ghost" user in
// the list for the entire session. Entries unseen for 2 minutes are removed.
//
// "Seen" (lastSeenByUser) is refreshed by every presence envelope for that
// user, and the server re-broadcasts each live connection's presence.update
// as a heartbeat every ping period (54 s, pingPeriod in
// internal/websocket/client.go), so an idle but connected collaborator is
// refreshed about twice per window and survives one lost heartbeat. Before
// the heartbeat an idle user was swept after 2 minutes and, since updates from
// unknown users were dropped, never came back. Keep PRESENCE_STALE_MS above
// twice that period (108 s).
//
// The local user (selfUserId) is exempt: the server never sends a connection
// its own heartbeat, so its entry is never refreshed. That leaves no ghost —
// the status handler clears every user on disconnect/error, and the next
// presence.sync rebuilds the list.
//
// The sweep is tied to the connection lifecycle (started in connectToFlow,
// stopped in disconnect) rather than a module-level setInterval, so the timer
// only runs while a flow is connected and is properly cleared on flow-close and
// logout — no leaked closure living for the page's lifetime.
const PRESENCE_STALE_MS = 120_000
let sweepTimer: ReturnType<typeof setInterval> | null = null

function startPresenceSweep() {
  if (sweepTimer != null) return
  sweepTimer = setInterval(() => {
    const state = usePresenceStore.getState()
    if (state.flowId == null) return
    const now = Date.now()
    let changed = false
    const users = {...state.users}
    for (const id of Object.keys(users)) {
      if (id === selfUserId) continue
      if (now - (lastSeenByUser.get(id) ?? 0) > PRESENCE_STALE_MS) {
        delete users[id]
        lastSeenByUser.delete(id)
        changed = true
      }
    }
    if (changed) usePresenceStore.setState({users})
  }, 60_000)
}

function stopPresenceSweep() {
  if (sweepTimer != null) {
    clearInterval(sweepTimer)
    sweepTimer = null
  }
}
