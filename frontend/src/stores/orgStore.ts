import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import { request } from '@/api/client'
import type { AuthUser } from '@/api/auth'
import { useFlowStore } from './flowStore'
import { useSearchStore } from './searchStore'
import { useAnalysisStore } from './analysisStore'
import { useChatStore } from './chatStore'

export type OrgRole = 'admin' | 'member' | 'viewer' | 'guest'

export interface OrgMember {
  userId: string
  role: OrgRole
  joinedAt: string
  user?: Pick<AuthUser, 'id' | 'email' | 'displayName' | 'avatarUrl'>
}

export interface Organisation {
  id: string
  name: string
  ownerId: string
  members: OrgMember[]
  createdAt: string
  updatedAt: string
}

interface OrgState {
  organisations: Organisation[]
  activeOrgId: string | null
  isLoading: boolean
  isBusy: boolean
  error: string | null

  loadOrgs: () => Promise<void>
  setActiveOrg: (id: string | null) => void
  createOrg: (name: string) => Promise<Organisation>
  inviteMember: (orgId: string, email: string, role: OrgRole) => Promise<void>
  removeMember: (orgId: string, userId: string) => Promise<void>
  setMemberRole: (orgId: string, userId: string, role: OrgRole) => Promise<void>
  deleteOrg: (orgId: string) => Promise<void>
  clearError: () => void
}

export const useOrgStore = create<OrgState>()(persist((set, get) => ({
  organisations: [],
  activeOrgId: null,
  isLoading: false,
  isBusy: false,
  error: null,

  loadOrgs: async () => {
    if (get().isLoading) return
    set({ isLoading: true, error: null })
    try {
      const orgs = await request<Organisation[]>('/api/orgs', undefined, 'GET')
      set(s => ({
        organisations: orgs ?? [],
        isLoading: false,
        // The persisted active org may have been deleted, or the user removed
        // from it (or this is a different account) — fall back to Personal.
        activeOrgId: s.activeOrgId && (orgs ?? []).some(o => o.id === s.activeOrgId)
          ? s.activeOrgId
          : null,
      }))
    } catch (err) {
      set({ isLoading: false, error: err instanceof Error ? err.message : 'Failed to load organisations' })
    }
  },

  setActiveOrg: (id) => {
    if (get().activeOrgId === id) return
    set({ activeOrgId: id })
    useFlowStore.getState().reset()
    useSearchStore.getState().clear()
    useAnalysisStore.getState().reset()
    useChatStore.setState({ threads: [], activeThreadId: null, conversations: new Map() })
  },

  createOrg: async (name) => {
    if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
    set({ isBusy: true, error: null })
    try {
      const org = await request<Organisation>('/api/orgs', { name })
      set(s => ({ organisations: [...s.organisations, org], isBusy: false }))
      return org
    } catch (err) {
      set({ isBusy: false, error: err instanceof Error ? err.message : 'Failed to create organisation' })
      throw err
    }
  },

  inviteMember: async (orgId, email, role) => {
    if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
    set({ isBusy: true, error: null })
    try {
      await request(`/api/orgs/${orgId}/members`, { email, role })
      await get().loadOrgs()
      set({ isBusy: false })
    } catch (err) {
      set({ isBusy: false, error: err instanceof Error ? err.message : 'Failed to invite member' })
      throw err
    }
  },

  removeMember: async (orgId, userId) => {
    if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
    set({ isBusy: true, error: null })
    try {
      await request(`/api/orgs/${orgId}/members/${userId}`, undefined, 'DELETE')
      await get().loadOrgs()
      set({ isBusy: false })
    } catch (err) {
      set({ isBusy: false, error: err instanceof Error ? err.message : 'Failed to remove member' })
      throw err
    }
  },

  setMemberRole: async (orgId, userId, role) => {
    if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
    set({ isBusy: true, error: null })
    try {
      await request(`/api/orgs/${orgId}/members/${userId}/role`, { role })
      await get().loadOrgs()
      set({ isBusy: false })
    } catch (err) {
      set({ isBusy: false, error: err instanceof Error ? err.message : 'Failed to update member role' })
      throw err
    }
  },

  deleteOrg: async (orgId) => {
    if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
    set({ isBusy: true, error: null })
    try {
      await request(`/api/orgs/${orgId}`, undefined, 'DELETE')
      set(s => ({
        organisations: s.organisations.filter(o => o.id !== orgId),
        activeOrgId: s.activeOrgId === orgId ? null : s.activeOrgId,
        isBusy: false,
      }))
    } catch (err) {
      set({ isBusy: false, error: err instanceof Error ? err.message : 'Failed to delete organisation' })
      throw err
    }
  },

  clearError: () => set({ error: null }),
}), {
  name: 'baki-active-org',
  partialize: s => ({ activeOrgId: s.activeOrgId }),
}))
