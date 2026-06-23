import { describe, it, expect, vi, afterEach } from 'vitest'
import { mockFetch } from '@/testing/testHelpers'

// Mock the platform adapter so the client resolves a backend URL/token.
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

// Re-import after mocking so each test gets fresh module/config state.
async function getAuthApi() {
  return (await import('./auth')).authApi
}

describe('authApi.changePassword wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  // Regression guard: the backend decodes the JSON key `oldPassword`
  // (internal/api/handlers_auth.go). Sending `currentPassword` left it empty and
  // made every change-password request fail with 401.
  it('posts the current password under the `oldPassword` key the backend expects', async () => {
    const fetchSpy = mockFetch({ status: 'ok' })
    const authApi = await getAuthApi()

    await authApi.changePassword('CurrentPass1!', 'BrandNewPass1!')

    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/auth/change-password')
    expect(init.method).toBe('POST')
    const body = JSON.parse(init.body as string)
    expect(body).toEqual({ oldPassword: 'CurrentPass1!', newPassword: 'BrandNewPass1!' })
    expect(body).not.toHaveProperty('currentPassword')
  })
})
