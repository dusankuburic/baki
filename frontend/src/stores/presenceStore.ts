import { create } from 'zustand'
import {
  collaborationService,
  type ConnectionStatus,
  type PresencePayload,
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

  connectToFlow: (flowId: string) => Promise<void>
  disconnect: () => void
  updateSelectedBlock: (blockId: string) => void
}

let cleanupHandlers: Array<() => void> = []

function teardown(): void {
  cleanupHandlers.forEach(fn => fn())
  cleanupHandlers = []
}

export const usePresenceStore = create<PresenceState>((set) => ({
  users: {},
  status: 'disconnected',
  flowId: null,

  connectToFlow: async (flowId: string) => {
    teardown()

    const cfg = await getBackendConfig()
    set({ flowId, users: {} })

    collaborationService.connect(flowId, cfg.apiUrl, getWsTicket)
    syncManager.start()

    cleanupHandlers.push(
      collaborationService.onStatusChange(status => set({ status })),
      collaborationService.subscribe(handleEnvelope),
    )
  },

  disconnect: () => {
    teardown()
    collaborationService.disconnect()
    syncManager.stop()
    set({ users: {}, status: 'disconnected', flowId: null })
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
  }

}
