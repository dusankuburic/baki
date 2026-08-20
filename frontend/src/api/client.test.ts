import {describe, it, expect, vi, afterEach} from 'vitest'
import {mockFetch} from '@/testing/testHelpers'

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
      const fetchSpy = mockFetch({result: 'ok'})
      const {request} = await getClient()

      await request('/api/test')

      expect(fetchSpy).toHaveBeenCalledOnce()
      const [url] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(url).toBe('http://localhost:9999/api/test')
    })

    it('sends the Bearer token in the Authorization header', async () => {
      const fetchSpy = mockFetch({result: 'ok'})
      const {request} = await getClient()

      await request('/api/test')

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer mock-token')
    })

    it('defaults to POST method', async () => {
      const fetchSpy = mockFetch({result: 'ok'})
      const {request} = await getClient()

      await request('/api/test', {body: {some: 'body'}})

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.method).toBe('POST')
    })

    it('respects explicit GET method and omits body', async () => {
      const fetchSpy = mockFetch([])
      const {request} = await getClient()

      await request('/api/list', {method: 'GET'})

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.method).toBe('GET')
      expect(init.body).toBeUndefined()
    })

    it('serialises the body as JSON', async () => {
      const fetchSpy = mockFetch({created: true})
      const {request} = await getClient()

      await request('/api/create', {body: {name: 'Test'}})

      const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
      expect(init.body).toBe(JSON.stringify({name: 'Test'}))
    })

    it('throws when the server returns a non-OK status', async () => {
      mockFetch({error: 'Not found'}, 404)
      const {request} = await getClient()

      await expect(request('/api/missing')).rejects.toThrow('Not found')
    })

    it('uses the standard envelope message ({code,message}) when present', async () => {
      // render.Error shape — the backend's primary error envelope.
      mockFetch({code: 'BAD_REQUEST', message: 'flowId is required', requestId: 'r-1'}, 400)
      const {request} = await getClient()

      await expect(request('/api/flow/apply-fix-batch')).rejects.toThrow('flowId is required')
    })

    it('falls back to "Request failed" when error body has no message', async () => {
      vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({}), {status: 500}))
      const {request} = await getClient()

      await expect(request('/api/broken')).rejects.toThrow('Request failed')
    })
  })

  describe('requestValidated()', () => {
    it('returns the parsed body when it matches the schema', async () => {
      mockFetch({token: 'abc'})
      const {requestValidated} = await getClient()
      const {z} = await import('zod')

      const out = await requestValidated('/api/x', z.object({token: z.string()}))

      expect(out).toEqual({token: 'abc'})
    })

    it('throws ResponseValidationError when the body fails the schema', async () => {
      mockFetch({notToken: 1})
      const {requestValidated, ResponseValidationError} = await getClient()
      const {z} = await import('zod')

      const schema = z.object({token: z.string()})
      await expect(requestValidated('/api/x', schema)).rejects.toBeInstanceOf(ResponseValidationError)
    })

    it('includes the failing path in the validation error message', async () => {
      mockFetch({})
      const {requestValidated} = await getClient()
      const {z} = await import('zod')

      await expect(requestValidated('/api/important', z.object({token: z.string()}))).rejects.toThrow('/api/important')
    })
  })

  // Regression: the AnalysisReport/Finding schemas MUST use .passthrough() so
  // backend fields not explicitly enumerated (autoFix, fingerprint, confidence,
  // autoFixHint, metadata, metrics, groups, ruleProfiles) survive validation.
  // Without passthrough, zod's default .strip() silently deletes them, breaking
  // the Apply-fix button, health-score badge, per-rule timing, and triage keys.
  describe('requestValidated() passthrough — no silent field stripping', () => {
    it('preserves Finding extra fields (autoFix, confidence, fingerprint, metadata)', async () => {
      const finding = {
        id: 'F-001',
        ruleId: 'hardcoded-credential',
        severity: 'error',
        title: 'Hardcoded credential',
        description: '...',
        blockId: 'b1',
        subflowId: 'sf1',
        autoFix: 'replace-with-variable',
        confidence: 'high',
        autoFixHint: 'Replace the literal with %API_KEY%.',
        fingerprint: 'hardcoded-credential:b1:ApiKey',
        metadata: {property: 'Value', variable: 'ApiKey'},
      }
      mockFetch(finding)
      const {requestValidated} = await getClient()
      const {FindingSchema} = await import('./schemas')

      const out = await requestValidated('/api/finding', FindingSchema)

      expect(out).toMatchObject(finding)
      // The previously-stripped fields must survive:
      expect(out).toHaveProperty('autoFix', 'replace-with-variable')
      expect(out).toHaveProperty('confidence', 'high')
      expect(out).toHaveProperty('fingerprint')
      expect(out).toHaveProperty('metadata')
    })

    it('preserves AnalysisReport extra fields (metrics, groups, ruleProfiles)', async () => {
      const report = {
        flowId: 'f1',
        generatedAt: '2024-01-01T00:00:00Z',
        durationMs: 42,
        findings: [],
        metrics: {healthScore: 87},
        groups: [{blockId: 'b1', count: 2}],
        ruleProfiles: [{ruleId: 'unhandled-error', durationMs: 5}],
      }
      mockFetch(report)
      const {requestValidated} = await getClient()
      const {AnalysisReportSchema} = await import('./schemas')

      const out = await requestValidated('/api/analysis/analyze', AnalysisReportSchema)

      expect(out).toHaveProperty('metrics')
      expect((out as {metrics?: {healthScore?: number}}).metrics?.healthScore).toBe(87)
      expect(out).toHaveProperty('groups')
      expect(out).toHaveProperty('ruleProfiles')
    })
  })

  describe('request() transient retry', () => {
    it('retries idempotent GET on 503 then succeeds', async () => {
      let calls = 0
      vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        calls++
        if (calls === 1) return new Response('temporarily unavailable', {status: 503})
        return new Response(JSON.stringify({ok: true}), {status: 200})
      })
      const {request} = await getClient()

      const out = await request('/api/x', {method: 'GET'})

      expect(out).toEqual({ok: true})
      expect(calls).toBe(2)
    })

    it('does NOT retry non-idempotent POST on 500', async () => {
      let calls = 0
      vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        calls++
        return new Response(JSON.stringify({error: 'boom'}), {status: 500})
      })
      const {request} = await getClient()

      await expect(request('/api/x', {body: {a: 1}, method: 'POST'})).rejects.toThrow('boom')
      expect(calls).toBe(1)
    })
  })
})
