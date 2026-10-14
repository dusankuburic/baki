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
