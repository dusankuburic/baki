import { describe, it, expect, vi, afterEach } from 'vitest'
import { mockFetch } from '@/testing/testHelpers'

// Mock the platform adapter so we control the returned config
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

// Re-import after mocking so the module gets the mocked adapter
// We use a dynamic import so each test file gets a fresh module state.
async function getClient() {
  // Reset module cache between tests by re-importing
  return import('./client')
}

describe('api/client', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    // Reset the module's cached config between test runs
    vi.resetModules()
  })

  describe('request()', () => {
    it('uses the adapter apiUrl as the base URL', async () => {
      const fetchSpy = mockFetch({ result: 'ok' })
      const { request } = await getClient()

      await request('/api/test')

      expect(fetchSpy).toHaveBeenCalledOnce()
      const [url] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(url).toBe('http://localhost:9999/api/test')
    })

    it('sends the Bearer token in the Authorization header', async () => {
      const fetchSpy = mockFetch({ result: 'ok' })
      const { request } = await getClient()

      await request('/api/test')

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer mock-token')
    })

    it('defaults to POST method', async () => {
      const fetchSpy = mockFetch({ result: 'ok' })
      const { request } = await getClient()

      await request('/api/test', { some: 'body' })

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.method).toBe('POST')
    })

    it('respects explicit GET method and omits body', async () => {
      const fetchSpy = mockFetch([])
      const { request } = await getClient()

      await request('/api/list', undefined, 'GET')

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.method).toBe('GET')
      expect(init.body).toBeUndefined()
    })

    it('serialises the body as JSON', async () => {
      const fetchSpy = mockFetch({ created: true })
      const { request } = await getClient()

      await request('/api/create', { name: 'Test' })

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.body).toBe(JSON.stringify({ name: 'Test' }))
    })

    it('throws when the server returns a non-OK status', async () => {
      mockFetch({ error: 'Not found' }, 404)
      const { request } = await getClient()

      await expect(request('/api/missing')).rejects.toThrow('Not found')
    })

    it('falls back to "Request failed" when error body has no message', async () => {
      vi.spyOn(globalThis, 'fetch').mockResolvedValue(
        new Response(JSON.stringify({}), { status: 500 })
      )
      const { request } = await getClient()

      await expect(request('/api/broken')).rejects.toThrow('Request failed')
    })
  })
})
