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
