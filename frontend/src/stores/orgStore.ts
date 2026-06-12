import { create } from 'zustand'
import { request } from '@/api/client'
import type { AuthUser } from '@/api/auth'

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

export const useOrgStore = create<OrgState>((set, get) => ({
  organisations: [],
  activeOrgId: null,
  isLoading: false,
  error: null,

  loadOrgs: async () => {
    set({ isLoading: true, error: null })
    try {
      const orgs = await request<Organisation[]>('/api/orgs', undefined, 'GET')
      set({ organisations: orgs ?? [], isLoading: false })
    } catch (err) {
      set({ isLoading: false, error: err instanceof Error ? err.message : 'Failed to load organisations' })
    }
  },

  setActiveOrg: (id) => set({ activeOrgId: id }),

  createOrg: async (name) => {
    const org = await request<Organisation>('/api/orgs', { name })
    set(s => ({ organisations: [...s.organisations, org] }))
    return org
  },

  inviteMember: async (orgId, email, role) => {
    await request(`/api/orgs/${orgId}/members`, { email, role })
    await get().loadOrgs()
  },

  removeMember: async (orgId, userId) => {
    await request(`/api/orgs/${orgId}/members/${userId}`, undefined, 'DELETE')
    await get().loadOrgs()
  },

  setMemberRole: async (orgId, userId, role) => {
    await request(`/api/orgs/${orgId}/members/${userId}/role`, { role })
    await get().loadOrgs()
  },

  deleteOrg: async (orgId) => {
    await request(`/api/orgs/${orgId}`, undefined, 'DELETE')
    set(s => ({
      organisations: s.organisations.filter(o => o.id !== orgId),
      activeOrgId: s.activeOrgId === orgId ? null : s.activeOrgId,
    }))
  },

  clearError: () => set({ error: null }),
}))
