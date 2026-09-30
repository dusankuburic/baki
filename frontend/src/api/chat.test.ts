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
