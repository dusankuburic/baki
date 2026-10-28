import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore, autoTitle, AUTO_TITLE_MAX} from '@/stores/chatStore'

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: () => ({
    registerStream: async () => true,
    confirmStream: vi.fn(),
    cancel: vi.fn(),
    teardownStream: vi.fn(),
    resumeInto: vi.fn(),
  }),
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    streamChatMessage: vi.fn().mockResolvedValue('stream-title'),
    cancelStream: vi.fn(),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
  },
  flowApi: {loadFlowFromPath: vi.fn().mockResolvedValue(null)},
  analysisApi: {analyzeFlowById: vi.fn().mockResolvedValue({flowId: 'flow-1', findings: []})},
}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

import {useChatStreamEngine} from './useChatStreamEngine'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

function seedThread(id: string) {
  const thread = {
    id,
    flowId: 'flow-1',
    title: '',
    createdAt: '2024',
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
  }
  useChatStore.setState(s => ({
    threads: [...s.threads, thread],
    conversations: new Map([...s.conversations, [id, []]]),
    historyStatus: {...s.historyStatus, [id]: {state: 'loaded' as const, token: 1}},
  }))
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    cb()
    return 0
  })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  useChatStore.setState({threads: [], streams: {}, activeThreadId: null, conversations: new Map(), historyStatus: {}})
})

// R-frontend-19: the thread title had two rules (40 characters in appendMessage, 50 in the engine's send); the
// engine no longer touches the title, so a send titles the thread exactly as an append does.
describe('thread auto-title', () => {
  it('a 60-character first message with newlines gets one title, the same via the store and the engine', async () => {
    const text = `${'A'.repeat(20)}\n\n${'B'.repeat(20)}\n${'C'.repeat(17)}`
    expect(text).toHaveLength(60)

    seedThread('viaStore')
    useChatStore.getState().appendMessage('viaStore', {id: 'u1', role: 'user', content: text, timestamp: 't'})

    seedThread('viaEngine')
    const {result} = renderHook(() => useChatStreamEngine())
    await act(async () => {
      expect(result.current.send('viaEngine', text, META)).toBe(true)
    })

    const titleOf = (id: string) => useChatStore.getState().threads.find(t => t.id === id)!.title
    expect(titleOf('viaEngine')).toBe(titleOf('viaStore'))
    expect(titleOf('viaEngine')).toBe(autoTitle(text))
    expect(titleOf('viaEngine')).toHaveLength(AUTO_TITLE_MAX)
    expect(titleOf('viaEngine')).not.toMatch(/\n/)
  })
})
