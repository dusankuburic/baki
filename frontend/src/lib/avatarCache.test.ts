import {describe, it, expect, vi, beforeEach, beforeAll} from 'vitest'

vi.mock('@/api/client', () => ({
  requestBlob: vi.fn(),
}))

import {requestBlob} from '@/api/client'
import {
  loadAvatar,
  resolveAvatarUrl,
  invalidateAvatar,
  clearAvatarCache,
  isInternalAvatarUrl,
  onAvatarsChanged,
  avatarCacheSize,
} from './avatarCache'

const revoked: string[] = []

beforeAll(() => {
  // jsdom ships no object URLs; the cache hands them out, so this suite supplies them.
  URL.createObjectURL = vi.fn(() => `blob:${Math.random()}`)
  URL.revokeObjectURL = vi.fn((url: string) => revoked.push(url))
})

beforeEach(() => {
  vi.clearAllMocks()
  clearAvatarCache()
  revoked.length = 0
})

describe('avatarCache', () => {
  it('marks the internal serving route and leaves external URLs alone', () => {
    expect(isInternalAvatarUrl('/api/users/u1/avatar')).toBe(true)
    expect(isInternalAvatarUrl('https://img.example.com/me.png')).toBe(false)
    expect(isInternalAvatarUrl(undefined)).toBe(false)
    expect(resolveAvatarUrl('https://img.example.com/me.png')).toBe('https://img.example.com/me.png')
    expect(resolveAvatarUrl(undefined)).toBeNull()
  })

  it('loads once, caches the object URL, and shares concurrent callers', async () => {
    vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
    const [a, b] = await Promise.all([loadAvatar('/api/users/u1/avatar'), loadAvatar('/api/users/u1/avatar')])
    expect(requestBlob).toHaveBeenCalledTimes(1)
    expect(a).toBe(b)
    expect(a).toMatch(/^blob:/)
    // The cached value serves later reads without a request.
    expect(await loadAvatar('/api/users/u1/avatar')).toBe(a)
    expect(requestBlob).toHaveBeenCalledTimes(1)
  })

  it('caches a 404 as no image instead of refetching per render', async () => {
    vi.mocked(requestBlob).mockRejectedValue(Object.assign(new Error('not found'), {status: 404}))
    expect(await loadAvatar('/api/users/u2/avatar')).toBeNull()
    expect(await loadAvatar('/api/users/u2/avatar')).toBeNull()
    expect(requestBlob).toHaveBeenCalledTimes(1)
    expect(resolveAvatarUrl('/api/users/u2/avatar')).toBeNull()
  })

  it('does not cache a transient failure: the next load retries', async () => {
    vi.mocked(requestBlob).mockRejectedValueOnce(Object.assign(new Error('bad gateway'), {status: 502}))
    expect(await loadAvatar('/api/users/u4/avatar')).toBeNull()
    vi.mocked(requestBlob).mockResolvedValueOnce(new Blob(['x']))
    expect(await loadAvatar('/api/users/u4/avatar')).toMatch(/^blob:/)
    expect(requestBlob).toHaveBeenCalledTimes(2)
  })

  it('refetches after the entry goes stale, revoking the old object URL', async () => {
    vi.useFakeTimers()
    try {
      vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
      const first = (await loadAvatar('/api/users/u5/avatar'))!
      vi.advanceTimersByTime(6 * 60 * 1000)
      const second = (await loadAvatar('/api/users/u5/avatar'))!
      expect(requestBlob).toHaveBeenCalledTimes(2)
      expect(second).not.toBe(first)
      expect(revoked).toEqual([first])
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps the stale image when the refresh fails', async () => {
    vi.useFakeTimers()
    try {
      vi.mocked(requestBlob).mockResolvedValueOnce(new Blob(['x']))
      const first = (await loadAvatar('/api/users/u6/avatar'))!
      vi.advanceTimersByTime(6 * 60 * 1000)
      vi.mocked(requestBlob).mockRejectedValueOnce(new Error('offline'))
      expect(await loadAvatar('/api/users/u6/avatar')).toBe(first)
      expect(resolveAvatarUrl('/api/users/u6/avatar')).toBe(first)
      expect(revoked).toEqual([])
    } finally {
      vi.useRealTimers()
    }
  })

  it('is bounded: the oldest entries are evicted and their object URLs revoked', async () => {
    vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
    for (let i = 0; i < 205; i++) await loadAvatar(`/api/users/bulk${i}/avatar`)
    expect(avatarCacheSize()).toBe(200)
    expect(revoked).toHaveLength(5)
    expect(resolveAvatarUrl('/api/users/bulk0/avatar')).toBeNull()
    expect(resolveAvatarUrl('/api/users/bulk204/avatar')).toMatch(/^blob:/)
  })

  it('invalidation revokes the object URL and notifies listeners', async () => {
    vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
    const url = (await loadAvatar('/api/users/u3/avatar'))!
    let notified = 0
    const unsub = onAvatarsChanged(() => notified++)
    invalidateAvatar('/api/users/u3/avatar')
    expect(revoked).toEqual([url])
    expect(notified).toBe(1)
    expect(resolveAvatarUrl('/api/users/u3/avatar')).toBeNull()
    unsub()
  })
})
