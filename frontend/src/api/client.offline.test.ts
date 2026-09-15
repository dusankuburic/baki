import {describe, it, expect, vi, afterEach} from 'vitest'
import {mockFetch} from '@/testing/testHelpers'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

function setOnline(v: boolean) {
  Object.defineProperty(window.navigator, 'onLine', {value: v, configurable: true})
}

describe('api/client while offline', () => {
  afterEach(() => {
    setOnline(true)
    vi.restoreAllMocks()
    vi.resetModules()
  })

  // A mutation while offline used to go to fetch and die with a bare
  // "Failed to fetch" (after retries, for idempotent verbs) while the offline
  // banner implied the change had been queued. Nothing queues edits. Fail
  // fast, and say the change was not saved.
  it('refuses a mutation without touching the network', async () => {
    setOnline(false)
    const fetchSpy = mockFetch({ok: true})
    const {request, ApiError, OFFLINE_CODE} = await import('./client')
    const err = await request('/api/flow/save', {method: 'POST', body: {}}).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as InstanceType<typeof ApiError>).code).toBe(OFFLINE_CODE)
    expect((err as Error).message).toMatch(/not saved/i)
    expect(fetchSpy).not.toHaveBeenCalled()

    const del = await request('/api/library/f1', {method: 'DELETE'}).catch((e: unknown) => e)
    expect((del as InstanceType<typeof ApiError>).code).toBe(OFFLINE_CODE)
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('still lets reads through (the cache or the network may answer)', async () => {
    setOnline(false)
    const fetchSpy = mockFetch({result: 'ok'})
    const {request} = await import('./client')
    await expect(request('/api/system/info', {method: 'GET'})).resolves.toEqual({result: 'ok'})
    expect(fetchSpy).toHaveBeenCalledOnce()
  })
})
