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

async function getChatApi() {
  return (await import('./chat')).chatApi
}

describe('chatApi.getConversation payload shapes', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('reads {"messages": null} as an empty history and keeps the stream id', async () => {
    mockFetch({messages: null, activeStreamId: 'live-1'})
    const chatApi = await getChatApi()
    await expect(chatApi.getConversation('flow-1', 't-1')).resolves.toEqual({messages: [], activeStreamId: 'live-1'})
  })

  it('still rejects a payload that is not a conversation', async () => {
    mockFetch({messages: 'nope'})
    const chatApi = await getChatApi()
    await expect(chatApi.getConversation('flow-1', 't-1')).rejects.toThrow('Unexpected conversation payload')
  })

  it('reads the object and the legacy bare-array forms', async () => {
    const msg = {id: 'm1', role: 'user', content: 'q', timestamp: '2024-01-01T00:00:00Z'}
    mockFetch({messages: [msg]})
    let chatApi = await getChatApi()
    await expect(chatApi.getConversation('flow-1', 't-1')).resolves.toMatchObject({messages: [{id: 'm1'}]})

    vi.restoreAllMocks()
    vi.resetModules()
    mockFetch([msg])
    chatApi = await getChatApi()
    await expect(chatApi.getConversation('flow-1', 't-1')).resolves.toMatchObject({messages: [{id: 'm1'}]})
  })

  it("keeps a stored answer's knowledge notice status through the load (R-knowledge-1)", async () => {
    mockFetch({
      messages: [{id: 'a1', role: 'assistant', content: 'answer', timestamp: 't', knowledge: 'no_embedder'}],
      version: 3,
    })
    const chatApi = await getChatApi()
    const load = await chatApi.getConversation('flow-1', 't-1')
    expect(load.messages[0]!.knowledge).toBe('no_embedder')
  })
})

describe('chatApi.streamChatMessage refused while the server restarts', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('rejects with a 503 ApiError coded SERVER_RESTARTING, its hint readable, and is not retried by the transport', async () => {
    const fetchSpy = mockFetch(
      {code: 'SERVER_RESTARTING', message: 'the server is restarting', details: {retryAfterSeconds: 2}},
      503,
    )
    const chatApi = await getChatApi()
    const {ApiError} = await import('./client')
    const {classifyChatFailure, retryAfterHint} = await import('@/lib/chatErrors')

    const err = await chatApi.streamChatMessage({flowId: 'flow-1', userMessage: 'q'} as never).catch(e => e)

    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({status: 503, code: 'SERVER_RESTARTING'})
    expect(classifyChatFailure(err.message, err.code)).toBe('serverRestarting')
    expect(retryAfterHint(err)).toBe(2)
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })
})

describe('chatApi conversation versions', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const bodyOf = (spy: ReturnType<typeof mockFetch>) => JSON.parse(String(spy.mock.calls[0]![1]!.body))

  it('reads the version off the load, and takes a load without one (an older server) as unversioned', async () => {
    mockFetch({messages: [], version: 7})
    let chatApi = await getChatApi()
    await expect(chatApi.getConversation('flow-1', 't-1')).resolves.toEqual({messages: [], version: 7})

    vi.restoreAllMocks()
    vi.resetModules()
    mockFetch({messages: []})
    chatApi = await getChatApi()
    const load = await chatApi.getConversation('flow-1', 't-1')
    expect(load).toEqual({messages: []})
    expect('version' in load).toBe(false)
  })

  it('ignores a version that is not a non-negative integer', async () => {
    for (const bad of [-1, 1.5, '3', null]) {
      vi.restoreAllMocks()
      vi.resetModules()
      mockFetch({messages: [], version: bad})
      const chatApi = await getChatApi()
      expect((await chatApi.getConversation('flow-1', 't-1')).version).toBeUndefined()
    }
  })

  it('sends baseVersion only when it has one, and reports the version the save answers', async () => {
    const spy = mockFetch({status: 'ok', version: 12})
    const chatApi = await getChatApi()
    await expect(chatApi.saveConversation('flow-1', 't-1', [], 11)).resolves.toEqual({version: 12})
    expect(bodyOf(spy)).toEqual({flowId: 'flow-1', provider: 't-1', messages: [], baseVersion: 11})

    vi.restoreAllMocks()
    vi.resetModules()
    const spy2 = mockFetch({status: 'ok'})
    const chatApi2 = await getChatApi()
    await expect(chatApi2.saveConversation('flow-1', 't-1', [])).resolves.toEqual({})
    expect(bodyOf(spy2)).not.toHaveProperty('baseVersion')
  })

  it('sends baseVersion 0: a new thread is saved against "nothing stored"', async () => {
    const spy = mockFetch({status: 'ok', version: 3})
    const chatApi = await getChatApi()
    await chatApi.saveConversation('flow-1', 't-1', [], 0)
    expect(bodyOf(spy)).toHaveProperty('baseVersion', 0)
  })

  it('sends baseVersion on clear when it has one', async () => {
    const spy = mockFetch({status: 'ok'})
    const chatApi = await getChatApi()
    await chatApi.clearConversation('flow-1', 't-1', 4)
    expect(bodyOf(spy)).toEqual({flowId: 'flow-1', provider: 't-1', baseVersion: 4})
  })

  it('surfaces the 409 with its code and the current version', async () => {
    mockFetch({code: 'CONVERSATION_CONFLICT', message: 'changed', details: {version: 9}}, 409)
    const chatApi = await getChatApi()
    const err = await chatApi.saveConversation('flow-1', 't-1', [], 3).catch(e => e)
    expect(err).toMatchObject({status: 409, code: 'CONVERSATION_CONFLICT', details: {version: 9}})
  })
})

describe('chatApi.cancelStream when the stream runs on a server that cannot be reached', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const unreachable = () =>
    new Response(
      JSON.stringify({code: 'REPLICA_UNREACHABLE', message: 'internal server error', details: {retryAfterSeconds: 2}}),
      {status: 503, headers: {'Content-Type': 'application/json'}},
    )
  const ok = () =>
    new Response(JSON.stringify({status: 'ok'}), {status: 200, headers: {'Content-Type': 'application/json'}})

  it('sends the stop again after the wait the server named, and resolves when it gets through', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(unreachable()).mockResolvedValueOnce(ok())
    const chatApi = await getChatApi()

    const done = chatApi.cancelStream('s1')
    await vi.advanceTimersByTimeAsync(0)
    expect(spy).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1_999)
    expect(spy).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    await expect(done).resolves.toBeUndefined()
    expect(spy).toHaveBeenCalledTimes(2)
    expect(JSON.parse(String(spy.mock.calls[1]![1]!.body))).toEqual({id: 's1'})
  })

  it('gives up after two more tries and rejects with the coded error', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async () => unreachable())
    const chatApi = await getChatApi()

    const done = chatApi.cancelStream('s1').catch(e => e)
    await vi.advanceTimersByTimeAsync(10_000)
    const err = await done
    expect(err).toMatchObject({status: 503, code: 'REPLICA_UNREACHABLE'})
    expect(spy).toHaveBeenCalledTimes(3)
  })

  it('does not retry any other failure', async () => {
    const spy = mockFetch({code: 'STREAM_NOT_FOUND', message: 'stream not found'}, 403)
    const chatApi = await getChatApi()
    await expect(chatApi.cancelStream('s1')).rejects.toMatchObject({status: 403})
    expect(spy).toHaveBeenCalledTimes(1)
  })
})

describe('chatApi.respondFixDecision wire shape (R-testing-7)', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  // The router test (internal/api TestApprovals_BatchOptOutThroughRouter) decodes exactly these keys; renaming one on
  // either side fails a test.
  it('posts the decision with the batch opt-outs as excludedItemIndices', async () => {
    const fetchSpy = mockFetch({status: 'ok'})
    const chatApi = await getChatApi()
    await chatApi.respondFixDecision('stream-1', 'proposal-1', true, [1])
    const [url, init] = fetchSpy.mock.calls[0]!
    expect(String(url)).toContain('/api/chat/fix-decision')
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      streamId: 'stream-1',
      proposalId: 'proposal-1',
      approved: true,
      excludedItemIndices: [1],
    })
  })
})

// R-frontend-18: the web client never begins a stream with /api/chat/begin (it sends clientStreamId and subscribes
// first), so the client keeps no path to it.
describe('chatApi without the legacy begin handshake', () => {
  afterEach(() => {
    vi.resetModules()
  })

  it('has no begin call', async () => {
    const chatApi = await getChatApi()
    expect(Object.keys(chatApi).filter(k => /^begin/i.test(k))).toEqual([])
  })
})

// R-streaming-c20: a stop or a decision refused by a rate limit never reached the stream: it is sent again after the
// wait the 429's Retry-After names, so the billed stream does not run on because a bucket was empty.
describe('chatApi stream control under a rate limit', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const limited = (retryAfter = '3') =>
    new Response(JSON.stringify({code: 'RATE_LIMITED', message: 'rate limit exceeded'}), {
      status: 429,
      headers: {'Content-Type': 'application/json', 'Retry-After': retryAfter},
    })
  const ok = () =>
    new Response(JSON.stringify({status: 'ok'}), {status: 200, headers: {'Content-Type': 'application/json'}})

  it('sends a rate-limited stop again after Retry-After', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(limited('3')).mockResolvedValueOnce(ok())
    const chatApi = await getChatApi()

    const done = chatApi.cancelStream('s1')
    await vi.advanceTimersByTimeAsync(2_999)
    expect(spy).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    await expect(done).resolves.toBeUndefined()
    expect(spy).toHaveBeenCalledTimes(2)
  })

  it('rejects with the 429 once its retries are spent', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async () => limited('1'))
    const chatApi = await getChatApi()

    const done = chatApi.cancelStream('s1').catch(e => e)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(await done).toMatchObject({status: 429})
    expect(spy.mock.calls.length).toBeGreaterThan(2)
  })

  it('sends a rate-limited fix decision again after Retry-After', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(limited('2')).mockResolvedValueOnce(ok())
    const chatApi = await getChatApi()

    const done = chatApi.respondFixDecision('s1', 'p1', true)
    await vi.advanceTimersByTimeAsync(2_000)
    await expect(done).resolves.toBeDefined()
    expect(spy).toHaveBeenCalledTimes(2)
  })
})

// R-frontend-c26 (c): the caller's own threads of a flow, listed by the server, and their titles.
describe('chatApi thread index', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('lists one page of threads, keeping what the index needs and dropping malformed rows', async () => {
    const spy = mockFetch({
      items: [
        {
          id: 't1',
          title: 'Retry loop',
          createdAt: '2026-10-01T00:00:00Z',
          updatedAt: '2026-10-02T00:00:00Z',
          messageCount: 4,
          version: 3,
        },
        {id: '', title: 'no id'},
        {id: 't2', createdAt: '2026-10-03T00:00:00Z', updatedAt: '2026-10-03T00:00:00Z', messageCount: 0, version: 1},
      ],
      nextCursor: 'c-2',
    })
    const chatApi = await getChatApi()
    const page = await chatApi.listThreads('flow-1', {limit: 50, cursor: 'c-1'})
    expect(JSON.parse(String(spy.mock.calls[0]![1]!.body))).toEqual({flowId: 'flow-1', limit: 50, cursor: 'c-1'})
    expect(String(spy.mock.calls[0]![0])).toMatch(/\/api\/chat\/threads$/)
    expect(page).toEqual({
      items: [
        {
          id: 't1',
          title: 'Retry loop',
          createdAt: '2026-10-01T00:00:00Z',
          updatedAt: '2026-10-02T00:00:00Z',
          messageCount: 4,
          version: 3,
        },
        {
          id: 't2',
          title: '',
          createdAt: '2026-10-03T00:00:00Z',
          updatedAt: '2026-10-03T00:00:00Z',
          messageCount: 0,
          version: 1,
        },
      ],
      nextCursor: 'c-2',
    })
  })

  it('reads an empty index (the desktop app, no accounts) as no threads', async () => {
    mockFetch({items: null})
    const chatApi = await getChatApi()
    await expect(chatApi.listThreads('flow-1')).resolves.toEqual({items: []})
  })

  it("sets a thread's title and hands back the version the server names", async () => {
    const spy = mockFetch({status: 'ok', version: 2})
    const chatApi = await getChatApi()
    await expect(chatApi.setThreadTitle('flow-1', 't1', 'Retry loop')).resolves.toEqual({version: 2})
    expect(JSON.parse(String(spy.mock.calls[0]![1]!.body))).toEqual({
      flowId: 'flow-1',
      conversationId: 't1',
      title: 'Retry loop',
    })
    vi.restoreAllMocks()
    mockFetch({status: 'ok'})
    await expect(chatApi.setThreadTitle('flow-1', 't1', '')).resolves.toEqual({})
  })
})
