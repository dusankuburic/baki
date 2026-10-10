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

  // A server that serialised an empty (nil) Go slice answers
  // {"messages": null}: that is an empty history, not a failed load — a
  // failed load blocks every save of the thread.
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

// The wire contract of a create refused while the server drains for a
// restart (agent F's side): a 503 envelope coded SERVER_RESTARTING, whose
// retry hint rides in details (the client does not see the Retry-After
// header). The engine retries such a create once (stream-engine/lifecycle).
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
    // A POST is never repeated below the engine: the one retry is the engine's.
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })
})
