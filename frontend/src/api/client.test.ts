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

    it('throws an ApiError carrying the envelope status, code and requestId', async () => {
      // Consumers branch on err.code (machine-readable) instead of regexing
      // the message — the message is masked on 5xx and may be reworded.
      mockFetch(
        {code: 'CHAT_CAPACITY_REACHED', message: 'too many chat responses running at once', requestId: 'r-42'},
        429,
      )
      const {request, ApiError} = await getClient()

      const err: unknown = await request('/api/chat/stream').catch(e => e)
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as InstanceType<typeof ApiError>
      expect(apiErr.status).toBe(429)
      expect(apiErr.code).toBe('CHAT_CAPACITY_REACHED')
      expect(apiErr.requestId).toBe('r-42')
      expect(apiErr.message).toBe('too many chat responses running at once')
    })

    it('omits code/requestId when the error body is a legacy {error} shape', async () => {
      mockFetch({error: 'legacy proxy message'}, 502)
      const {request, ApiError} = await getClient()

      const err: unknown = await request('/api/legacy').catch(e => e)
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as InstanceType<typeof ApiError>
      expect(apiErr.status).toBe(502)
      expect(apiErr.code).toBeNull()
      expect(apiErr.requestId).toBeNull()
      expect(apiErr.message).toBe('legacy proxy message')
    })

    it('maps 403 to PermissionDeniedError and 409 to VersionConflictError (both ApiErrors)', async () => {
      mockFetch({code: 'FORBIDDEN', message: 'nope'}, 403)
      const {request, PermissionDeniedError, ApiError} = await getClient()
      const forbidden: unknown = await request('/api/a').catch(e => e)
      expect(forbidden).toBeInstanceOf(PermissionDeniedError)
      expect(forbidden).toBeInstanceOf(ApiError)

      mockFetch({code: 'CONFLICT', message: 'version mismatch'}, 409)
      const conflict: unknown = await request('/api/b').catch(e => e)
      expect(conflict).toBeInstanceOf(ApiError)
      expect((conflict as InstanceType<typeof ApiError>).status).toBe(409)
      expect((conflict as InstanceType<typeof ApiError>).code).toBe('CONFLICT')
    })
  })

  describe('GET dedup + micro-cache', () => {
    it('concurrent identical GETs share one round trip', async () => {
      const fetchSpy = mockFetch({items: []})
      const {request} = await getClient()

      const [a, b] = await Promise.all([
        request('/api/recent', {method: 'GET'}),
        request('/api/recent', {method: 'GET'}),
      ])

      expect(a).toEqual({items: []})
      expect(b).toEqual({items: []})
      expect(fetchSpy).toHaveBeenCalledOnce()
    })

    it('serves a repeat GET from the TTL cache within 5s', async () => {
      const fetchSpy = mockFetch({ts: 1})
      const {request} = await getClient()

      await request('/api/info', {method: 'GET'})
      const second = await request('/api/info', {method: 'GET'})

      expect(fetchSpy).toHaveBeenCalledOnce()
      expect(second).toEqual({ts: 1})
    })

    it('TTL hits return a private copy — in-place mutation cannot poison other consumers', async () => {
      mockFetch({items: [{id: 3}, {id: 1}, {id: 2}]})
      const {request} = await getClient()

      const first = await request<{items: {id: number}[]}>('/api/list', {method: 'GET'})
      first.items.sort((a, b) => a.id - b.id) // consumer A mutates its copy in place

      const second = await request<{items: {id: number}[]}>('/api/list', {method: 'GET'})
      expect(second).not.toBe(first) // separate object, not a shared reference
      expect(second.items).toEqual([{id: 3}, {id: 1}, {id: 2}]) // pristine server order
    })

    it('in-flight sharers each get their own copy of the result', async () => {
      mockFetch({items: [3, 1, 2]})
      const {request} = await getClient()

      const [a, b] = await Promise.all([
        request<{items: number[]}>('/api/shared', {method: 'GET'}),
        request<{items: number[]}>('/api/shared', {method: 'GET'}),
      ])

      expect(a).not.toBe(b) // shared round trip, not a shared object
      expect(a.items).toEqual([3, 1, 2])
      expect(b.items).toEqual([3, 1, 2])
    })

    // Regression (epoch guard): a GET that resolves after clearRequestCache()
    // (logout mid-flight / org switch) must not write the previous session's
    // response back into the freshly-cleared cache.
    it('a GET resolving after clearRequestCache does not re-populate the cache', async () => {
      let resolveFetch!: (body: string) => void
      const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(
        () =>
          new Promise(resolve => {
            resolveFetch = body => resolve(new Response(body, {status: 200}))
          }),
      )
      const {request, clearRequestCache} = await getClient()

      const pending = request<{account: string}>('/api/session-data', {method: 'GET'})
      // Let the request reach the (suspended) fetch call before "logout".
      await new Promise(r => setTimeout(r, 0))
      clearRequestCache()
      resolveFetch(JSON.stringify({account: 'previous-user'}))

      const first = await pending
      expect(first).toEqual({account: 'previous-user'}) // initiator still gets its response

      // The next account's request must hit the network, not the stale cache.
      fetchSpy.mockImplementation(async () => new Response(JSON.stringify({account: 'next-user'}), {status: 200}))
      const second = await request<{account: string}>('/api/session-data', {method: 'GET'})
      expect(fetchSpy).toHaveBeenCalledTimes(2)
      expect(second).toEqual({account: 'next-user'})
    })

    it('POSTs are never deduped or cached', async () => {
      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockImplementation(async () => new Response(JSON.stringify({ok: true}), {status: 200}))
      const {request} = await getClient()

      await request('/api/analysis/analyze')
      await request('/api/analysis/analyze')

      expect(fetchSpy).toHaveBeenCalledTimes(2)
    })

    it('a failed GET is not cached — the next call refetches', async () => {
      let call = 0
      const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        call++
        // 404 (non-transient) so the retry layer doesn't mask the failure.
        if (call === 1) return new Response(JSON.stringify({message: 'missing'}), {status: 404})
        return new Response(JSON.stringify({ok: true}), {status: 200})
      })
      const {request} = await getClient()

      await expect(request('/api/flaky', {method: 'GET'})).rejects.toThrow('missing')
      const out = await request('/api/flaky', {method: 'GET'})
      expect(out).toEqual({ok: true})
      expect(fetchSpy).toHaveBeenCalledTimes(2)
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
      const {getFindingSchema} = await import('./schemas')

      const out = await requestValidated('/api/finding', await getFindingSchema())

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
        stats: {errors: 0, warnings: 0, info: 0, blocksAnalyzed: 10, rulesRun: 29},
        metrics: {healthScore: 87},
        groups: [{blockId: 'b1', count: 2}],
        ruleProfiles: [{ruleId: 'unhandled-error', durationMs: 5}],
      }
      mockFetch(report)
      const {requestValidated} = await getClient()
      const {getAnalysisReportSchema} = await import('./schemas')

      const out = await requestValidated('/api/analysis/analyze', await getAnalysisReportSchema())

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

  describe('request() consumer cancellation (signal)', () => {
    it('rejects with AbortError when the consumer aborts mid-flight', async () => {
      // fetch that never settles on its own — only the abort path resolves it.
      vi.spyOn(globalThis, 'fetch').mockImplementation(
        (_url, init) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')),
            )
          }),
      )
      const {request} = await getClient()
      const controller = new AbortController()

      const pending = request('/api/slow', {method: 'GET', signal: controller.signal})
      controller.abort()

      const err: unknown = await pending.catch(e => e)
      // Consumers branch on err.name (documented contract); instanceof is
      // cross-realm-fragile under jsdom.
      expect((err as {name?: string}).name).toBe('AbortError')
    })

    it('short-circuits without touching the network when the signal is already aborted', async () => {
      const fetchSpy = mockFetch({ok: true})
      const {request} = await getClient()
      const controller = new AbortController()
      controller.abort()

      await expect(request('/api/x', {method: 'GET', signal: controller.signal})).rejects.toThrow()

      expect(fetchSpy).not.toHaveBeenCalled()
    })

    it('does not write an aborted GET into the micro-cache', async () => {
      vi.spyOn(globalThis, 'fetch').mockImplementation(
        (_url, init) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')),
            )
          }),
      )
      const {request} = await getClient()
      const controller = new AbortController()

      const pending = request('/api/cached-abort', {method: 'GET', signal: controller.signal})
      controller.abort()
      await pending.catch(() => {})

      // A later GET on the same path must hit the network (nothing cached).
      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockImplementation(async () => new Response(JSON.stringify({fresh: true}), {status: 200}))
      const out = await request('/api/cached-abort', {method: 'GET'})
      expect(fetchSpy).toHaveBeenCalledTimes(1)
      expect(out).toEqual({fresh: true})
    })

    it('abortAllRequests() aborts in-flight requests', async () => {
      vi.spyOn(globalThis, 'fetch').mockImplementation(
        (_url, init) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')),
            )
          }),
      )
      const {request, abortAllRequests} = await getClient()

      const pending = request('/api/batch', {body: {ids: [1]}, timeoutMs: 300_000})
      // Let the request reach doFetch (config resolution registers its
      // controller) before aborting — mirrors a real logout a moment later.
      await new Promise(r => setTimeout(r, 0))
      abortAllRequests()

      const err: unknown = await pending.catch(e => e)
      expect((err as {name?: string}).name).toBe('AbortError')
    })
  })
})
