import { create } from 'zustand'
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
      collaborationService.onStatusChange(status => set({ status })),
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
          [env.userId!]: { ...existing, ...p },
        },
      }
    })
  } else if (env.type === 'flow.changed') {
    const p = env.payload as FlowChangedPayload
    usePresenceStore.setState({ remoteVersion: p.version })
    flowChangeListeners.forEach(fn => fn(p.version))
  }

}
