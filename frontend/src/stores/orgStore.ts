import {getErrorMessage} from '@/lib/getErrorMessage'
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
  inviteMember: (orgId: string, email: string, role: OrgRole) => Promise<string | undefined>
  acceptInvite: (token: string) => Promise<void>
  removeMember: (orgId: string, userId: string) => Promise<{sharedCredentialDisabled: boolean}>
  setMemberRole: (orgId: string, userId: string, role: OrgRole) => Promise<{sharedCredentialDisabled: boolean}>
  deleteOrg: (orgId: string) => Promise<void>
  clearError: () => void
}

function resetOrgScopedState() {
  clearRequestCache()
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
            activeOrgId: s.activeOrgId && (orgs ?? []).some(o => o.id === s.activeOrgId) ? s.activeOrgId : null,
          }))
        } catch (err) {
          set({isLoading: false, error: getErrorMessage(err, 'Failed to load organisations')})
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
          set({isBusy: false, error: getErrorMessage(err, 'Failed to create organisation')})
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
          set({isBusy: false, error: getErrorMessage(err, 'Failed to rename organisation')})
          throw err
        }
      },

      inviteMember: async (orgId, email, role) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const res = await request<{token?: string} | undefined>(`/api/orgs/${orgId}/invites`, {body: {email, role}})
          set({isBusy: false})
          return res?.token
        } catch (err) {
          set({isBusy: false, error: getErrorMessage(err, 'Failed to send invite')})
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
          set({isBusy: false, error: getErrorMessage(err, 'Failed to accept invite')})
          throw err
        }
      },

      removeMember: async (orgId, userId) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const wasActive = get().activeOrgId === orgId
          const reply = await request<{status?: string; sharedCredentialDisabled?: boolean}>(
            `/api/orgs/${orgId}/members/${userId}`,
            {method: 'DELETE'},
          )
          await get().loadOrgs()
          if (wasActive && get().activeOrgId === null) resetOrgScopedState()
          set({isBusy: false})
          return {sharedCredentialDisabled: reply?.sharedCredentialDisabled === true}
        } catch (err) {
          set({isBusy: false, error: getErrorMessage(err, 'Failed to remove member')})
          throw err
        }
      },

      setMemberRole: async (orgId, userId, role) => {
        if (get().isBusy) return Promise.reject(new Error('Another operation is in progress'))
        set({isBusy: true, error: null})
        try {
          const reply = await request<{status?: string; sharedCredentialDisabled?: boolean}>(
            `/api/orgs/${orgId}/members/${userId}/role`,
            {body: {role}, method: 'PUT'},
          )
          await get().loadOrgs()
          set({isBusy: false})
          return {sharedCredentialDisabled: reply?.sharedCredentialDisabled === true}
        } catch (err) {
          set({isBusy: false, error: getErrorMessage(err, 'Failed to update member role')})
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
          if (wasActive) resetOrgScopedState()
        } catch (err) {
          set({isBusy: false, error: getErrorMessage(err, 'Failed to delete organisation')})
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

registerStoreReset(() => {
  useOrgStore.setState({organisations: [], activeOrgId: null, isLoading: false, isBusy: false, error: null})
})
