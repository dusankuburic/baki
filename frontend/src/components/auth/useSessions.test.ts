import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useSessions} from './useSessions'
import {authApi} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    listSessions: vi.fn(),
    revokeSession: vi.fn(),
  },
}))

vi.mock('@/stores/authStore', () => ({
  getCurrentSessionId: () => 'current-jti',
}))

beforeEach(() => {
  vi.clearAllMocks()
})

describe('useSessions', () => {
  it('marks the session matching the decoded refresh-token jti as current', async () => {
    vi.mocked(authApi.listSessions).mockResolvedValue([
      {id: 'current-jti', createdAt: '', expiresAt: ''},
      {id: 'other-jti', createdAt: '', expiresAt: ''},
    ])
    const {result} = renderHook(() => useSessions())
    await waitFor(() => expect(result.current.loading).toBe(false))

    expect(result.current.currentSessionId).toBe('current-jti')
    expect(result.current.sessions).toHaveLength(2)
  })

  it('revokeOthers signs out every session except the current one', async () => {
    vi.mocked(authApi.listSessions).mockResolvedValue([
      {id: 'current-jti', createdAt: '', expiresAt: ''},
      {id: 'other-1', createdAt: '', expiresAt: ''},
      {id: 'other-2', createdAt: '', expiresAt: ''},
    ])
    vi.mocked(authApi.revokeSession).mockResolvedValue(undefined)
    const {result} = renderHook(() => useSessions())
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.revokeOthers()
    })

    expect(authApi.revokeSession).toHaveBeenCalledTimes(2)
    expect(authApi.revokeSession).toHaveBeenCalledWith('other-1')
    expect(authApi.revokeSession).toHaveBeenCalledWith('other-2')
    expect(result.current.sessions).toEqual([{id: 'current-jti', createdAt: '', expiresAt: ''}])
  })

  it('keeps sessions that failed to revoke and surfaces an error', async () => {
    vi.mocked(authApi.listSessions).mockResolvedValue([
      {id: 'current-jti', createdAt: '', expiresAt: ''},
      {id: 'other-1', createdAt: '', expiresAt: ''},
      {id: 'other-2', createdAt: '', expiresAt: ''},
    ])
    vi.mocked(authApi.revokeSession).mockImplementation((id: string) =>
      id === 'other-1' ? Promise.resolve() : Promise.reject(new Error('network')),
    )
    const {result} = renderHook(() => useSessions())
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.revokeOthers()
    })

    expect(result.current.sessions.map(s => s.id).sort()).toEqual(['current-jti', 'other-2'])
    expect(result.current.error).toMatch(/some sessions/i)
  })

  it('is a no-op when there are no other sessions to revoke', async () => {
    vi.mocked(authApi.listSessions).mockResolvedValue([{id: 'current-jti', createdAt: '', expiresAt: ''}])
    const {result} = renderHook(() => useSessions())
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.revokeOthers()
    })

    expect(authApi.revokeSession).not.toHaveBeenCalled()
  })
})
