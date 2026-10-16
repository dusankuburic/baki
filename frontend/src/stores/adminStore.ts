import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {RoleFilter, SystemHealth} from '@/api/admin'
import {EMPTY_TRAIL, recordEnd, recordPage, type PageTrail} from '@/lib/pageTrail'

export const USERS_PAGE_SIZE = 10
export const USERS_PAGE_SIZES = [10, 25, 50]
export const AUDIT_PAGE_SIZE = 15
export const AUDIT_PAGE_SIZES = [15, 30, 50]

export interface UsersListState {
  q: string
  role: RoleFilter
  page: number
  pageSize: number
}

export interface AuditListState {
  action: string
  userId: string
  page: number
  pageSize: number
}

interface AdminState {
  users: UsersListState
  audit: AuditListState
  nonce: number
  usersNonce: number
  pendingSection: string | null
  pending: number
  lastUpdated: number | null
  health: SystemHealth | null
  // The cursors the user list's and audit log's pages answered, kept across remounts (the page is): a page past the
  // server's offset cap is reached only by one.
  usersTrail: PageTrail
  auditTrail: PageTrail

  setUsersQuery: (q: string) => void
  setUsersRole: (role: RoleFilter) => void
  setUsersPage: (page: number) => void
  setUsersPageSize: (size: number) => void
  clearUsersFilters: () => void
  setAuditAction: (action: string) => void
  setAuditUserId: (userId: string) => void
  setAuditPage: (page: number) => void
  setAuditPageSize: (size: number) => void
  clearAuditFilters: () => void
  focusUserActivity: (userId: string) => void
  consumeSection: () => string | null
  refresh: () => void
  touchUsers: () => void
  beginLoad: () => void
  endLoad: () => void
  markUpdated: () => void
  setHealth: (health: SystemHealth | null) => void
  noteUsersPage: (key: string, page: number, next: string | undefined) => void
  noteAuditPage: (key: string, page: number, next: string | undefined, sentCursor: boolean) => void
  noteAuditEnd: (key: string, page: number) => void
  reset: () => void
}

const USERS_DEFAULT: UsersListState = {q: '', role: '', page: 0, pageSize: USERS_PAGE_SIZE}
const AUDIT_DEFAULT: AuditListState = {action: '', userId: '', page: 0, pageSize: AUDIT_PAGE_SIZE}

const DEFAULTS = {
  users: USERS_DEFAULT,
  audit: AUDIT_DEFAULT,
  nonce: 0,
  usersNonce: 0,
  pendingSection: null as string | null,
  pending: 0,
  lastUpdated: null as number | null,
  health: null as SystemHealth | null,
  usersTrail: EMPTY_TRAIL,
  auditTrail: EMPTY_TRAIL,
}

export const useAdminStore = create<AdminState>()((set, get) => ({
  ...DEFAULTS,

  setUsersQuery: q => set(s => ({users: {...s.users, q, page: 0}})),
  setUsersRole: role => set(s => ({users: {...s.users, role, page: 0}})),
  setUsersPage: page => set(s => ({users: {...s.users, page: Math.max(0, page)}})),
  setUsersPageSize: pageSize => set(s => ({users: {...s.users, pageSize, page: 0}})),
  clearUsersFilters: () => set(s => ({users: {...s.users, q: '', role: '', page: 0}})),
  setAuditAction: action => set(s => ({audit: {...s.audit, action, page: 0}})),
  setAuditUserId: userId => set(s => ({audit: {...s.audit, userId, page: 0}})),
  setAuditPage: page => set(s => ({audit: {...s.audit, page: Math.max(0, page)}})),
  setAuditPageSize: pageSize => set(s => ({audit: {...s.audit, pageSize, page: 0}})),
  clearAuditFilters: () => set(s => ({audit: {...s.audit, action: '', userId: '', page: 0}})),
  focusUserActivity: userId => set(s => ({audit: {...s.audit, userId, page: 0}, pendingSection: 'admin-audit'})),
  consumeSection: () => {
    const id = get().pendingSection
    if (id) set({pendingSection: null})
    return id
  },
  // A refresh may find events past the page found empty.
  refresh: () => set(s => ({nonce: s.nonce + 1, auditTrail: {...s.auditTrail, end: undefined}})),
  touchUsers: () => set(s => ({usersNonce: s.usersNonce + 1})),
  beginLoad: () => set(s => ({pending: s.pending + 1})),
  endLoad: () => set(s => ({pending: Math.max(0, s.pending - 1)})),
  markUpdated: () => set({lastUpdated: Date.now()}),
  setHealth: health => set({health}),
  noteUsersPage: (key, page, next) => set(s => ({usersTrail: recordPage(s.usersTrail, key, page, next)})),
  noteAuditPage: (key, page, next, sentCursor) =>
    set(s => ({auditTrail: recordPage(s.auditTrail, key, page, next, sentCursor)})),
  noteAuditEnd: (key, page) => set(s => ({auditTrail: recordEnd(s.auditTrail, key, page)})),
  reset: () => set(DEFAULTS),
}))

registerStoreReset(() => useAdminStore.getState().reset())
