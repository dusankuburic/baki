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

const lastSeenByUser = new Map<string, number>()

let selfUserId: string | null = null

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
        if (status !== 'connected') synced = false
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

function nonEmptyString(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined
}

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
    const p = env.payload as Partial<PresenceSyncPayload> | null | undefined
    if (!p || !Array.isArray(p.users)) return
    synced = true
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
    if (!env.userId) return
    const userId = env.userId
    const p = (env.payload ?? {}) as Partial<PresencePayload>
    const selectedBlockId = peerSelection(userId, p.selectedBlockId)
    lastSeenByUser.set(userId, Date.now())
    usePresenceStore.setState(s => {
      const existing = s.users[userId]
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

registerStoreReset(() => {
  usePresenceStore.getState().disconnect()
  syncManager.reset()
})

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
