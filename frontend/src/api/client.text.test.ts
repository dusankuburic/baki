import {describe, it, expect, vi, afterEach} from 'vitest'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

const xml = '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites tests="1" failures="1"></testsuites>\n'

function respond(body: string, status: number, contentType: string) {
  return vi
    .spyOn(globalThis, 'fetch')
    .mockImplementation(async () => new Response(body, {status, headers: {'Content-Type': contentType}}))
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('requestText', () => {
  it('returns an XML body as text, where request() refuses it', async () => {
    const fetchSpy = respond(xml, 200, 'application/xml; charset=utf-8')
    const {request, requestText} = await import('./client')

    await expect(requestText('/api/analysis/export/junit', {body: {flowId: 'f1'}})).resolves.toBe(xml)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://localhost:9999/api/analysis/export/junit')
    expect(init.method).toBe('POST')
    expect(init.body).toBe(JSON.stringify({flowId: 'f1'}))
    expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer mock-token')

    await expect(request('/api/analysis/export/junit', {body: {flowId: 'f1'}})).rejects.toThrow('non-JSON')
  })

  it('keeps a JSON document byte for byte (no parse and re-stringify)', async () => {
    const sarif =
      '{\n  "version": "2.1.0",\n  "runs": [{"results": [{"message": {"text": "\\u003cb\\u003e \\u0026"}}]}]\n}\n'
    respond(sarif, 200, 'application/sarif+json')
    const {requestText} = await import('./client')
    await expect(requestText('/api/analysis/export/sarif', {body: {flowId: ''}})).resolves.toBe(sarif)
  })

  it('maps the error envelope as request() does', async () => {
    respond('{"code":"NOT_FOUND","message":"flow not found","requestId":"r-1"}', 404, 'application/json')
    const client = await import('./client')
    const err = await client
      .requestText('/api/analysis/export/junit', {body: {flowId: 'gone'}})
      .catch((e: unknown) => e)
    expect(err).toBeInstanceOf(client.ApiError)
    expect(err).toMatchObject({status: 404, code: 'NOT_FOUND', requestId: 'r-1', message: 'flow not found'})

    vi.restoreAllMocks()
    respond('{"code":"FORBIDDEN","message":"no"}', 403, 'application/json')
    await expect(client.requestText('/api/x', {body: {}})).rejects.toBeInstanceOf(client.PermissionDeniedError)
  })

  it('defaults to GET without a body', async () => {
    const fetchSpy = respond('plain', 200, 'text/plain')
    const {requestText} = await import('./client')
    await expect(requestText('/api/plain')).resolves.toBe('plain')
    const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('GET')
    expect(init.body).toBeUndefined()
  })
})

describe('requestBlob', () => {
  it('takes a body for a POST download', async () => {
    const fetchSpy = respond('%PDF-1.7', 200, 'application/pdf')
    const {requestBlob} = await import('./client')
    const blob = await requestBlob('/api/download', {body: {id: 1}})
    expect(await blob.text()).toBe('%PDF-1.7')
    const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('POST')
    expect(init.body).toBe(JSON.stringify({id: 1}))
  })
})

describe('requestForm', () => {
  it('posts the file as a multipart part under the field name, with the bearer and no JSON content type', async () => {
    const fetchSpy = respond('{"id":"u1","avatarUrl":"/api/users/u1/avatar"}', 200, 'application/json')
    const {requestForm} = await import('./client')
    const out = await requestForm<{avatarUrl: string}>('/api/auth/profile/avatar', new Blob(['png'], {type: 'image/png'}))
    expect(out.avatarUrl).toBe('/api/users/u1/avatar')
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://localhost:9999/api/auth/profile/avatar')
    expect(init.method).toBe('POST')
    const headers = init.headers as Record<string, string>
    expect(headers['Authorization']).toBe('Bearer mock-token')
    expect(Object.keys(headers).map(h => h.toLowerCase())).not.toContain('content-type')
    expect(init.body).toBeInstanceOf(FormData)
    expect((init.body as FormData).get('avatar')).toBeInstanceOf(Blob)
  })

  it('throws the server error instead of parsing its body as the result', async () => {
    respond('{"error":"avatar upload exceeds the size limit"}', 413, 'application/json')
    const {requestForm} = await import('./client')
    await expect(requestForm('/api/auth/profile/avatar', new Blob(['x']))).rejects.toMatchObject({status: 413})
  })
})
