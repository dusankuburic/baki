import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {persist} from 'zustand/middleware'
import {request, clearRequestCache} from '@/api/client'
import type {AuthUser} from '@/api/auth'
import {useFlowStore} from './flowStore'
import {useAnalysisStore} from './analysisStore'
import {useChatStore} from './chatStore'

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
  renameOrg: (orgId: string, name: string) => Promise<void>
  // Resolves to the raw invite token so the UI can show a copyable link
  // when the invite email cannot be delivered (no SMTP configured).
  inviteMember: (orgId: string, email: string, role: OrgRole) => Promise<string | undefined>
  acceptInvite: (token: string) => Promise<void>
  // Also how a member leaves: the server lets a non-admin remove their own row.
  removeMember: (orgId: string, userId: string) => Promise<void>
  setMemberRole: (orgId: string, userId: string, role: OrgRole) => Promise<void>
  deleteOrg: (orgId: string) => Promise<void>
  clearError: () => void
}

// Flow, analysis and chat state is loaded under the active org but not keyed by
// it, so leaving that org context must wipe it in full.
function resetOrgScopedState() {
  // Drop any 5s-TTL GET cache from the previous org context: safe today
  // because org-scoped endpoints are path-parameterized, but a future
  // org-scoped-by-header GET would otherwise leak across the switch within the
  // TTL window (same rationale as logout's clear).
  clearRequestCache()
  // Deliberately broader than resetDerivedStateForFlow: analysis reports and
  // chat threads aren't org-scoped in frontend state, so an org switch must
  // wipe them in full.
  useFlowStore.getState().reset()
  useAnalysisStore.getState().reset()
  useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map(), streams: {}, drafts: {}})
}

export const useOrgStore = create<OrgState>()(
  persist(
    (set, get) => ({
      organisations: [],
      activeOrgId: null,
      isLoading: false,
      isBusy: false,
      error: null,

      loadOrgs: async () => {
        if (get().isLoading) return
        set({isLoading: true, error: null})
        try {
          const orgs = await request<Organisation[]>('/api/orgs', {method: 'GET'})
          set(s => ({
            organisations: orgs ?? [],
            isLoading: false,
            // The persisted active org may have been deleted, or the user removed
            // from it (or this is a different account) — fall back to Personal.
            activeOrgId: s.activeOrgId && (orgs ?? []).some(o => o.id === s.activeOrgId) ? s.activeOrgId : null,
          }))
        } catch (err) {
          set({isLoading: false, error: err instanceof Error ? err.message : 'Failed to load organisations'})
        }
      },

      setActiveOrg: id => {
        if (get().activeOrgId === id) return
        set({activeOrgId: id})
        resetOrgScopedState()
      },

      createOrg: async name => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const org = await request<Organisation>('/api/orgs', {body: {name}})
          set(s => ({organisations: [...s.organisations, org], isBusy: false}))
          return org
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to create organisation'})
          throw err
        }
      },

      renameOrg: async (orgId, name) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const updated = await request<Organisation>(`/api/orgs/${orgId}`, {body: {name}, method: 'PUT'})
          set(s => ({
            organisations: s.organisations.map(o => (o.id === orgId ? {...o, ...updated} : o)),
            isBusy: false,
          }))
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to rename organisation'})
          throw err
        }
      },

      inviteMember: async (orgId, email, role) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          // Create a token invite — the backend emails the invitee a link they
          // accept via acceptInvite (POST /api/invites/{token}/accept). The member
          // list only changes once they accept, so no loadOrgs() here.
          const res = await request<{token?: string} | undefined>(`/api/orgs/${orgId}/invites`, {body: {email, role}})
          set({isBusy: false})
          return res?.token
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to send invite'})
          throw err
        }
      },

      acceptInvite: async token => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const org = await request<Organisation>(`/api/invites/${token}/accept`, {})
          await get().loadOrgs()
          if (org?.id) set({activeOrgId: org.id})
          set({isBusy: false})
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to accept invite'})
          throw err
        }
      },

      removeMember: async (orgId, userId) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const wasActive = get().activeOrgId === orgId
          await request(`/api/orgs/${orgId}/members/${userId}`, {method: 'DELETE'})
          await get().loadOrgs()
          // The caller left (or was otherwise dropped from) the org they were
          // working in: loadOrgs already fell back to Personal, but that only
          // rewrites activeOrgId — the org's open flow and findings would stay
          // on screen behind requests that now 403.
          if (wasActive && get().activeOrgId === null) resetOrgScopedState()
          set({isBusy: false})
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to remove member'})
          throw err
        }
      },

      setMemberRole: async (orgId, userId, role) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          await request(`/api/orgs/${orgId}/members/${userId}/role`, {body: {role}, method: 'PUT'})
          await get().loadOrgs()
          set({isBusy: false})
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to update member role'})
          throw err
        }
      },

      deleteOrg: async orgId => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const wasActive = get().activeOrgId === orgId
          await request(`/api/orgs/${orgId}`, {method: 'DELETE'})
          set(s => ({
            organisations: s.organisations.filter(o => o.id !== orgId),
            activeOrgId: s.activeOrgId === orgId ? null : s.activeOrgId,
            isBusy: false,
          }))
          // Deleting the org you are working in leaves its flow, findings and
          // chat on screen behind requests that now 404 — same reason the
          // leave path resets, and the org is gone for good here.
          if (wasActive) resetOrgScopedState()
        } catch (err) {
          set({isBusy: false, error: err instanceof Error ? err.message : 'Failed to delete organisation'})
          throw err
        }
      },

      clearError: () => set({error: null}),
    }),
    {
      name: 'baki-active-org',
      partialize: s => ({activeOrgId: s.activeOrgId}),
    },
  ),
)

// Reset on logout (see storeRegistry).
registerStoreReset(() =>
  useOrgStore.setState({organisations: [], activeOrgId: null, isLoading: false, isBusy: false, error: null}),
)
