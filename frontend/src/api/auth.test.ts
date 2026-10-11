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

async function getAuthApi() {
  return (await import('./auth')).authApi
}

describe('authApi.changePassword wire contract', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('posts the current password under the `oldPassword` key the backend expects', async () => {
    const fetchSpy = mockFetch({status: 'ok'})
    const authApi = await getAuthApi()

    await authApi.changePassword('CurrentPass1!', 'BrandNewPass1!')

    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/auth/change-password')
    expect(init.method).toBe('POST')
    const body = JSON.parse(init.body as string)
    expect(body).toEqual({oldPassword: 'CurrentPass1!', newPassword: 'BrandNewPass1!'})
    expect(body).not.toHaveProperty('currentPassword')
  })
})
