import {describe, it, expect, beforeEach} from 'vitest'
import {useAdminStore, USERS_PAGE_SIZE, AUDIT_PAGE_SIZE} from './adminStore'

beforeEach(() => {
  useAdminStore.getState().reset()
})

describe('adminStore', () => {
  it('pages users by 10 and the audit log by 15 by default', () => {
    const s = useAdminStore.getState()
    expect(USERS_PAGE_SIZE).toBe(10)
    expect(AUDIT_PAGE_SIZE).toBe(15)
    expect(s.users).toEqual({q: '', role: '', page: 0, pageSize: 10})
    expect(s.audit).toEqual({action: '', userId: '', page: 0, pageSize: 15})
  })

  it('restarts from the first page when a filter or the page size changes', () => {
    const s = useAdminStore.getState()
    s.setUsersPage(3)
    s.setUsersQuery('ali')
    expect(useAdminStore.getState().users).toMatchObject({q: 'ali', page: 0})
    s.setUsersPage(2)
    s.setUsersRole('member')
    expect(useAdminStore.getState().users).toMatchObject({role: 'member', page: 0})
    s.setUsersPage(2)
    s.setUsersPageSize(25)
    expect(useAdminStore.getState().users).toMatchObject({pageSize: 25, page: 0})

    s.setAuditPage(4)
    s.setAuditAction('auth.login')
    expect(useAdminStore.getState().audit).toMatchObject({action: 'auth.login', page: 0})
    s.setAuditPage(4)
    s.setAuditUserId('u1')
    expect(useAdminStore.getState().audit).toMatchObject({userId: 'u1', page: 0})
    s.setAuditPage(1)
    s.setAuditPageSize(30)
    expect(useAdminStore.getState().audit).toMatchObject({pageSize: 30, page: 0})
  })

  it('never pages below zero', () => {
    const s = useAdminStore.getState()
    s.setUsersPage(-2)
    s.setAuditPage(-1)
    expect(useAdminStore.getState().users.page).toBe(0)
    expect(useAdminStore.getState().audit.page).toBe(0)
  })

  it('clears the filters without touching the page size', () => {
    const s = useAdminStore.getState()
    s.setUsersPageSize(50)
    s.setUsersQuery('x')
    s.setUsersRole('admin')
    s.clearUsersFilters()
    expect(useAdminStore.getState().users).toEqual({q: '', role: '', page: 0, pageSize: 50})
    s.setAuditPageSize(30)
    s.setAuditAction('auth.login')
    s.setAuditUserId('u1')
    s.clearAuditFilters()
    expect(useAdminStore.getState().audit).toEqual({action: '', userId: '', page: 0, pageSize: 30})
  })

  it('focusUserActivity filters the audit log by that user and asks the page to scroll there', () => {
    const s = useAdminStore.getState()
    s.setAuditPage(2)
    s.focusUserActivity('u7')
    expect(useAdminStore.getState().audit).toMatchObject({userId: 'u7', page: 0})
    expect(useAdminStore.getState().pendingSection).toBe('admin-audit')
    expect(s.consumeSection()).toBe('admin-audit')
    expect(useAdminStore.getState().pendingSection).toBeNull()
    expect(s.consumeSection()).toBeNull()
  })

  it('tracks refreshes, in-flight requests and the last update, and reset restores everything', () => {
    const s = useAdminStore.getState()
    s.refresh()
    s.touchUsers()
    s.beginLoad()
    s.beginLoad()
    s.endLoad()
    s.markUpdated()
    s.setHealth({database: {status: 'ok'}, blob: {status: 'ok'}, redis: {status: 'skipped'}, overall: 'ok'})
    expect(useAdminStore.getState()).toMatchObject({nonce: 1, usersNonce: 1, pending: 1})
    expect(useAdminStore.getState().lastUpdated).not.toBeNull()
    expect(useAdminStore.getState().health?.overall).toBe('ok')
    s.endLoad()
    s.endLoad()
    expect(useAdminStore.getState().pending).toBe(0)

    s.setUsersQuery('q')
    s.reset()
    expect(useAdminStore.getState()).toMatchObject({
      users: {q: '', role: '', page: 0, pageSize: 10},
      audit: {action: '', userId: '', page: 0, pageSize: 15},
      nonce: 0,
      usersNonce: 0,
      pendingSection: null,
      pending: 0,
      lastUpdated: null,
      health: null,
    })
  })
})
