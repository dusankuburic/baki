import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {RoleFilter, SystemHealth} from '@/api/admin'

// List state for the Admin Dashboard: which page of users and of the audit
// log is open, with which filters. Pure presentation — nothing is fetched
// here (the admin hooks read this and fetch) and nothing is persisted, so a
// filter survives switching views within a session and is dropped on logout.
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
  /** Bumped by the header Refresh: every admin hook refetches. */
  nonce: number
  /** Bumped after a role change: the user list and the stats refetch. */
  usersNonce: number
  /** A section another part of the page asked to scroll to once rendered. */
  pendingSection: string | null
  /** Admin requests in flight, for the page's progress bar. */
  pending: number
  /** When an admin request last delivered data (ms since epoch). */
  lastUpdated: number | null
  /** The last health check, shared by the overview tile and Operations. */
  health: SystemHealth | null

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
  /** Filters the audit log by a user and asks the page to scroll there. */
  focusUserActivity: (userId: string) => void
  consumeSection: () => string | null
  refresh: () => void
  touchUsers: () => void
  beginLoad: () => void
  endLoad: () => void
  markUpdated: () => void
  setHealth: (health: SystemHealth | null) => void
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
}

export const useAdminStore = create<AdminState>()((set, get) => ({
  ...DEFAULTS,

  // Any filter or page-size change restarts from the first page.
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
  refresh: () => set(s => ({nonce: s.nonce + 1})),
  touchUsers: () => set(s => ({usersNonce: s.usersNonce + 1})),
  beginLoad: () => set(s => ({pending: s.pending + 1})),
  endLoad: () => set(s => ({pending: Math.max(0, s.pending - 1)})),
  markUpdated: () => set({lastUpdated: Date.now()}),
  setHealth: health => set({health}),
  reset: () => set(DEFAULTS),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => useAdminStore.getState().reset())
