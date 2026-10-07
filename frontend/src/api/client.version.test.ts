import {describe, it, expect, vi, afterEach} from 'vitest'

// The API client hands every response's build version to lib/buildVersion,
// whatever the status — the reload banner's only input.
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

const noted = vi.hoisted(() => vi.fn())
vi.mock('@/lib/buildVersion', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/buildVersion')>()),
  noteServerVersion: noted,
}))

import {request, requestBlob, clearRequestCache} from './client'

afterEach(() => {
  vi.restoreAllMocks()
  noted.mockClear()
  clearRequestCache()
})

const respond = (body: string, status: number, version?: string) =>
  vi
    .spyOn(globalThis, 'fetch')
    .mockImplementation(async () => new Response(body, {status, headers: version ? {'X-Baki-Version': version} : {}}))

describe('the client reads the server build off every response', () => {
  it('a success, an error and a blob alike', async () => {
    respond('{"ok":true}', 200, 'v2.0.0')
    await request('/api/system/info', {method: 'GET', noDedup: true})
    expect(noted).toHaveBeenLastCalledWith('v2.0.0')

    vi.restoreAllMocks()
    respond('{"message":"nope"}', 404, 'v2.0.1')
    await expect(request('/api/missing', {method: 'GET', noDedup: true})).rejects.toThrow('nope')
    expect(noted).toHaveBeenLastCalledWith('v2.0.1')

    vi.restoreAllMocks()
    respond('bytes', 200, 'v2.0.2')
    await requestBlob('/api/export/x')
    expect(noted).toHaveBeenLastCalledWith('v2.0.2')
  })

  it('passes null when a response carries no version', async () => {
    respond('{}', 200)
    await request('/api/system/info', {method: 'GET', noDedup: true})
    expect(noted).toHaveBeenLastCalledWith(null)
  })
})
