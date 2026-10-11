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

async function getClient() {
  return import('./client')
}

describe('api/client', () => {
  afterEach(() => {
    vi.restoreAllMocks()
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

    it('resolves a 204 No Content as undefined instead of a non-JSON failure', async () => {
      vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, {status: 204}))
      const {request} = await getClient()

      await expect(request('/api/thing/1', {method: 'DELETE', body: {}})).resolves.toBeUndefined()
    })

    it('throws when the server returns a non-OK status', async () => {
      mockFetch({error: 'Not found'}, 404)
      const {request} = await getClient()

      await expect(request('/api/missing')).rejects.toThrow('Not found')
    })

    it('uses the standard envelope message ({code,message}) when present', async () => {
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
      first.items.sort((a, b) => a.id - b.id)

      const second = await request<{items: {id: number}[]}>('/api/list', {method: 'GET'})
      expect(second).not.toBe(first)
      expect(second.items).toEqual([{id: 3}, {id: 1}, {id: 2}])
    })

    it('in-flight sharers each get their own copy of the result', async () => {
      mockFetch({items: [3, 1, 2]})
      const {request} = await getClient()

      const [a, b] = await Promise.all([
        request<{items: number[]}>('/api/shared', {method: 'GET'}),
        request<{items: number[]}>('/api/shared', {method: 'GET'}),
      ])

      expect(a).not.toBe(b)
      expect(a.items).toEqual([3, 1, 2])
      expect(b.items).toEqual([3, 1, 2])
    })

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
      await new Promise(r => setTimeout(r, 0))
      clearRequestCache()
      resolveFetch(JSON.stringify({account: 'previous-user'}))

      const first = await pending
      expect(first).toEqual({account: 'previous-user'})

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

  describe('GET micro-cache size gate', () => {
    function mockFetchSequence(bodies: unknown[]) {
      let call = 0
      return vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        const body = bodies[Math.min(call++, bodies.length - 1)]
        return new Response(JSON.stringify(body), {status: 200, headers: {'Content-Type': 'application/json'}})
      })
    }

    afterEach(() => {
      vi.unstubAllGlobals()
      vi.useRealTimers()
    })

    it('a body above the gate is neither cached nor cloned, so the next GET sees the server again', async () => {
      const clone = vi.fn((v: unknown) => JSON.parse(JSON.stringify(v)))
      vi.stubGlobal('structuredClone', clone)
      const {request, GET_CACHE_MAX_BODY_CHARS, requestCacheSize} = await getClient()
      const big = {rev: 1, blocks: 'x'.repeat(1024 * 1024)}
      const fetchSpy = mockFetchSequence([big, {...big, rev: 2}])

      const first = await request<{rev: number}>('/api/library/f1/content', {method: 'GET'})
      const second = await request<{rev: number}>('/api/library/f1/content', {method: 'GET'})

      expect(first.rev).toBe(1)
      expect(second.rev).toBe(2)
      expect(fetchSpy).toHaveBeenCalledTimes(2)
      expect(clone).not.toHaveBeenCalled()
      expect(requestCacheSize()).toBe(0)
      expect(big.blocks.length).toBeGreaterThan(GET_CACHE_MAX_BODY_CHARS)
    })

    it('a small body is still served from the cache (one fetch, cloned per consumer)', async () => {
      const clone = vi.fn((v: unknown) => JSON.parse(JSON.stringify(v)))
      vi.stubGlobal('structuredClone', clone)
      const {request, requestCacheSize} = await getClient()
      const fetchSpy = mockFetchSequence([{rev: 1}, {rev: 2}])

      const first = await request<{rev: number}>('/api/small', {method: 'GET'})
      const second = await request<{rev: number}>('/api/small', {method: 'GET'})

      expect(first.rev).toBe(1)
      expect(second.rev).toBe(1)
      expect(second).not.toBe(first)
      expect(fetchSpy).toHaveBeenCalledOnce()
      expect(clone).toHaveBeenCalled()
      expect(requestCacheSize()).toBe(1)
    })

    it('an expired entry is evicted when it is read, not only when another GET writes', async () => {
      vi.useFakeTimers()
      const {request, requestCacheSize} = await getClient()
      let call = 0
      vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        call++
        if (call === 1) return new Response(JSON.stringify({rev: 1}), {status: 200})
        return new Response(JSON.stringify({message: 'gone'}), {status: 404})
      })

      await request('/api/expiring', {method: 'GET'})
      expect(requestCacheSize()).toBe(1)
      vi.advanceTimersByTime(6_000)
      expect(requestCacheSize()).toBe(1)

      await expect(request('/api/expiring', {method: 'GET'})).rejects.toThrow('gone')
      expect(call).toBe(2)
      expect(requestCacheSize()).toBe(0)
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

      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockImplementation(async () => new Response(JSON.stringify({fresh: true}), {status: 200}))
      const out = await request('/api/cached-abort', {method: 'GET'})
      expect(fetchSpy).toHaveBeenCalledTimes(1)
      expect(out).toEqual({fresh: true})
    })

    it('never shares an abortable GET: aborting one leaves an identical concurrent GET intact', async () => {
      let calls = 0
      vi.spyOn(globalThis, 'fetch').mockImplementation(
        (_url, init) =>
          new Promise<Response>((resolve, reject) => {
            calls++
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')),
            )
            setTimeout(() => resolve(new Response(JSON.stringify({items: [1]}), {status: 200})), 5)
          }),
      )
      const {request} = await getClient()
      const first = new AbortController()
      const second = new AbortController()

      const a = request('/api/library', {method: 'GET', signal: first.signal})
      const b = request('/api/library', {method: 'GET', signal: second.signal})
      await new Promise(r => setTimeout(r, 0))
      expect(calls).toBe(2)
      first.abort()

      const err: unknown = await a.catch(e => e)
      expect((err as {name?: string}).name).toBe('AbortError')
      await expect(b).resolves.toEqual({items: [1]})
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
      await new Promise(r => setTimeout(r, 0))
      abortAllRequests()

      const err: unknown = await pending.catch(e => e)
      expect((err as {name?: string}).name).toBe('AbortError')
    })
  })
})

type Failure = Error & {code?: string | null; status?: number}

async function failureOf(promise: Promise<unknown>): Promise<Failure> {
  try {
    await promise
  } catch (e) {
    return e as Failure
  }
  throw new Error('expected the request to reject')
}

describe('network failures', () => {
  it('words a failed connection on a mutation without retrying it', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('Failed to fetch'))
    const {request, ApiError, NETWORK_CODE} = await getClient()

    const err = await failureOf(request('/api/chat/stream', {body: {userMessage: 'hi'}}))
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe(NETWORK_CODE)
    expect(err.status).toBe(0)
    expect(err.message).toMatch(/Couldn't reach the server/)
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })

  it('retries a read, then words the failure', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('Failed to fetch'))
    const {request, NETWORK_CODE} = await getClient()

    const err = await failureOf(request('/api/flow/recent', {method: 'GET'}))
    expect(err.code).toBe(NETWORK_CODE)
    expect(fetchSpy.mock.calls.length).toBeGreaterThan(1)
  })

  it('leaves a consumer abort untouched', async () => {
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('Failed to fetch'))
    const {request, NETWORK_CODE} = await getClient()
    const controller = new AbortController()
    controller.abort()

    const err = await failureOf(request('/api/test', {signal: controller.signal}))
    expect(err.name).toBe('AbortError')
    expect(err.code).not.toBe(NETWORK_CODE)
  })
})
