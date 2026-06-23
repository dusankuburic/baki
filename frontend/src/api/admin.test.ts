import { describe, it, expect, vi, afterEach } from 'vitest'
import { mockFetch } from '@/testing/testHelpers'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

async function getAdminApi() {
  return (await import('./admin')).adminApi
}

describe('adminApi.setUserRole wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  // Regression: the route is registered PUT-only (routes_chi.go). The default
  // POST returned 405 and admin role changes silently failed.
  it('sends PUT to /api/admin/users/{id}/role', async () => {
    const fetchSpy = mockFetch({ status: 'ok' })
    const adminApi = await getAdminApi()

    await adminApi.setUserRole('user-42', 'admin')

    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/admin/users/user-42/role')
    expect(init.method).toBe('PUT')
    expect(JSON.parse(init.body as string)).toEqual({ role: 'admin' })
  })
})
