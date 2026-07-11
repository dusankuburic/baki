import { create } from 'zustand'
import { registerStoreReset } from './storeRegistry'
import {
  collaborationService,
  type ConnectionStatus,
  type PresencePayload,
  type FlowChangedPayload,
  type Envelope,
} from '@/services/collaboration/CollaborationService'
import { syncManager } from '@/services/sync/SyncManager'
import { getBackendConfig, getWsTicket } from '@/api/client'

export interface PresenceUser {
  userId: string
  displayName: string
  avatarUrl?: string
  selectedBlockId?: string
  lastSeen?: number
}

interface PresenceState {
  users: Record<string, PresenceUser>
  status: ConnectionStatus
  flowId: string | null
  remoteVersion: number

  connectToFlow: (flowId: string) => Promise<void>
  disconnect: () => void
  updateSelectedBlock: (blockId: string) => void
}

let flowChangeListeners: Array<(version: number) => void> = []
let cleanupHandlers: Array<() => void> = []
let generation = 0

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
}

export const usePresenceStore = create<PresenceState>((set) => ({
  users: {},
  status: 'disconnected',
  flowId: null,
  remoteVersion: 0,

  connectToFlow: async (flowId: string) => {
    teardown()

    const gen = generation
    const cfg = await getBackendConfig()

    if (gen !== generation) return

    set({ flowId, users: {}, remoteVersion: 0 })

    collaborationService.connect(flowId, cfg.apiUrl, getWsTicket)
    syncManager.start(flowId)

    cleanupHandlers.push(
      collaborationService.onStatusChange(status => {
        set({ status })
        // Clear stale users on disconnect/error so the UI doesn't
        // show ghosts from before the connection dropped. Fresh presence
        // data will repopulate as other users re-announce.
        if (status === 'disconnected' || status === 'error') {
          set({ users: {} })
        }
      }),
      collaborationService.subscribe(handleEnvelope),
    )
  },

  disconnect: () => {
    teardown()
    collaborationService.disconnect()
    syncManager.stop()
    set({ users: {}, status: 'disconnected', flowId: null, remoteVersion: 0 })
  },

  updateSelectedBlock: (blockId: string) => {
    syncManager.enqueue({
      type: 'presence.update',
      payload: { selectedBlockId: blockId } satisfies Partial<PresencePayload>,
    })
  },
}))

function handleEnvelope(env: Envelope): void {
  if (env.type === 'presence.join') {
    const p = env.payload as PresencePayload
    usePresenceStore.setState(s => ({
      users: {
        ...s.users,
        [p.userId]: {
          userId: p.userId,
          displayName: p.displayName ?? p.userId,
          avatarUrl: p.avatarUrl,
          selectedBlockId: p.selectedBlockId,
          lastSeen: Date.now(),
        },
      },
    }))
  } else if (env.type === 'presence.leave') {
    if (!env.userId) return
    usePresenceStore.setState(s => {
      const users = { ...s.users }
      delete users[env.userId!]
      return { users }
    })
  } else if (env.type === 'presence.update') {
    const p = env.payload as Partial<PresencePayload>
    if (!env.userId) return
    usePresenceStore.setState(s => {
      const existing = s.users[env.userId!]
      if (!existing) return s
      return {
        users: {
          ...s.users,
          [env.userId!]: { ...existing, ...p, lastSeen: Date.now() },
        },
      }
    })
  } else if (env.type === 'flow.changed') {
    const p = env.payload as FlowChangedPayload
    usePresenceStore.setState({ remoteVersion: p.version })
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
const PRESENCE_STALE_MS = 120_000
setInterval(() => {
  const now = Date.now()
  const state = usePresenceStore.getState()
  let changed = false
  const users = { ...state.users }
  for (const [id, user] of Object.entries(users)) {
    if (now - (user.lastSeen ?? 0) > PRESENCE_STALE_MS) {
      delete users[id]
      changed = true
    }
  }
  if (changed) usePresenceStore.setState({ users })
}, 60_000)
