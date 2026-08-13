import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'

// StreamHandler is not exported from useStreamingMessage, so define a local
// type matching its shape for the captured handler.
interface StreamHandler {
  onChunk: (text: string, streamId: string) => void
  onReplace: (text: string, streamId: string) => void
  onDone: (tokensOut: number, tokensIn: number, streamId: string) => void
  onError: (error: string, streamId: string) => void
  onToolStatus?: (label: string, streamId: string) => void
  onAppend?: (delta: string, streamId: string) => void
  getAccLength?: (streamId: string) => number
}

let capturedHandler: StreamHandler | null = null

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: (handler: StreamHandler) => {
    capturedHandler = handler
    return {
      registerStream: vi.fn(),
      cancel: vi.fn(),
      teardownStream: vi.fn(),
    }
  },
}))

vi.mock('@/api', () => ({
  chatApi: {
    streamMessage: vi.fn(),
    cancelStream: vi.fn(),
    saveConversation: vi.fn().mockResolvedValue(undefined),
  },
}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

import {useChatStreamEngine} from './useChatStreamEngine'
import {chatApi} from '@/api'
import type {FlowDocument} from '@/types'

const mockDoc = {id: 'flow-1', name: 'Test', subflows: []} as unknown as FlowDocument

function renderEngine() {
  return renderHook(() =>
    useChatStreamEngine({
      doc: mockDoc,
      provider: 'claude' as never,
      selectedModel: 'claude-sonnet-4',
      getMessages: (threadId: string) => useChatStore.getState().conversations.get(threadId) ?? [],
    }),
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    cb()
    return 0
  })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  useChatStore.setState({
    threads: [],
    streams: {},
    activeThreadId: null,
    conversations: new Map(),
  })
})

describe('useChatStreamEngine', () => {
  describe('concurrent streams', () => {
    it('keeps stream A text separate from stream B', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA')
        result.current.beginAcc('streamB', 'threadB')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
            threadB: {streamId: 'streamB', messageId: 'mB', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
      })
      act(() => {
        capturedHandler!.onChunk('Hello from A', 'streamA')
        capturedHandler!.onChunk('Hello from B', 'streamB')
        capturedHandler!.onChunk(' more A', 'streamA')
      })
      const streams = useChatStore.getState().streams
      expect(streams.threadA.text).toBe('Hello from A more A')
      expect(streams.threadB.text).toBe('Hello from B')
    })

    it('ignores chunks for an unknown streamId', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
      })
      act(() => {
        capturedHandler!.onChunk('ghost', 'unknown-stream')
      })
      expect(useChatStore.getState().streams.threadA.text).toBe('')
    })
  })

  describe('stale generation discard', () => {
    it('isCurrentGen returns false for a superseded generation', () => {
      const {result} = renderEngine()
      let gen1: number, gen2: number
      act(() => {
        gen1 = result.current.bumpGen('threadA')
        gen2 = result.current.bumpGen('threadA')
      })
      expect(gen1!).toBe(1)
      expect(gen2!).toBe(2)
      expect(result.current.isCurrentGen('threadA', gen1!)).toBe(false)
      expect(result.current.isCurrentGen('threadA', gen2!)).toBe(true)
    })

    it('isCurrentGen is independent per thread', () => {
      const {result} = renderEngine()
      let genA1: number, genB1: number
      act(() => {
        genA1 = result.current.bumpGen('threadA')
        genB1 = result.current.bumpGen('threadB')
        result.current.bumpGen('threadA')
      })
      expect(result.current.isCurrentGen('threadA', genA1!)).toBe(false)
      expect(result.current.isCurrentGen('threadB', genB1!)).toBe(true)
    })
  })

  describe('getAccLength (UTF-8 byte length)', () => {
    it('returns byte length for non-ASCII', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
        capturedHandler!.onChunk('héllo→😀', 'streamA')
      })
      expect(capturedHandler!.getAccLength!('streamA')).toBe(13)
    })

    it('returns 0 for an unknown streamId', () => {
      renderEngine()
      expect(capturedHandler!.getAccLength!('unknown')).toBe(0)
    })
  })

  describe('stopAndCommit', () => {
    it('does NOT commit when no text was generated', () => {
      const {result} = renderEngine()
      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
        })
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
      })
      act(() => {
        result.current.stopAndCommit('streamA', 'threadA', 'mA')
      })
      expect(useChatStore.getState().conversations.get('threadA')).toHaveLength(0)
    })
  })

  describe('onDone slot-mismatch guard', () => {
    it('does not commit when the store slot streamId does not match', () => {
      const {result} = renderEngine()
      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
        })
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamB', messageId: 'mB', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
        capturedHandler!.onChunk('text', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(5, 10, 'streamA')
      })
      expect(useChatStore.getState().conversations.get('threadA')).toHaveLength(0)
    })
  })

  describe('onError', () => {
    it('commits an error message with finishReason error', () => {
      const {result} = renderEngine()
      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
        })
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
      })
      act(() => {
        capturedHandler!.onError('provider down', 'streamA')
      })
      const msgs = useChatStore.getState().conversations.get('threadA')
      expect(msgs).toHaveLength(1)
      expect(msgs![0].finishReason).toBe('error')
      expect(msgs![0].content).toContain('provider down')
    })
  })

  describe('onDone slot-mismatch guard', () => {
    it('does not commit when the store slot streamId does not match', () => {
      const {result} = renderEngine()
      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
        })
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {streamId: 'streamB', messageId: 'mB', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
        capturedHandler!.onChunk('text', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(5, 10, 'streamA')
      })
      expect(useChatStore.getState().conversations.get('threadA')).toHaveLength(0)
    })
  })

  describe('commitAssistantMessage (direct)', () => {
    it('appends a message to conversations when called directly', () => {
      const {result} = renderEngine()
      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
        })
      })
      act(() => {
        result.current.commitAssistantMessage('threadA', 'mA', 'hello world', {finishReason: 'interrupted'})
      })
      // appendMessage writes to the conversations Map, not threads[].messages
      const msgs = useChatStore.getState().conversations.get('threadA')
      expect(msgs).toHaveLength(1)
      expect(msgs![0].content).toBe('hello world')
      expect(msgs![0].finishReason).toBe('interrupted')
    })
  })

  // Regression: a stream's done event can land after the user has switched to a
  // different flow. The conversation must be persisted under the thread's own
  // flowId (immutable), NOT docRef.current (which now points at the new flow) —
  // otherwise flow A's AI conversation is saved under flow B.
  describe('commitAssistantMessage doc-switch safety', () => {
    it('saves under the thread flowId, not the switched-to doc', () => {
      const docA = {id: 'flow-A', name: 'A', subflows: []} as unknown as FlowDocument
      const docB = {id: 'flow-B', name: 'B', subflows: []} as unknown as FlowDocument

      const {result, rerender} = renderHook(
        ({doc}: {doc: FlowDocument}) =>
          useChatStreamEngine({
            doc,
            provider: 'claude' as never,
            selectedModel: 'claude-sonnet-4',
            getMessages: (threadId: string) => useChatStore.getState().conversations.get(threadId) ?? [],
          }),
        {initialProps: {doc: docA}},
      )

      act(() => {
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-A',
              title: 'T',
              createdAt: '2024',
              contextBlockId: 'block1',
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
          streams: {
            threadA: {streamId: 'streamA', messageId: 'mA', text: '', isThinking: true, tokens: 0, toolStatus: null},
          },
        })
        result.current.beginAcc('streamA', 'threadA')
        capturedHandler!.onChunk('hello from A', 'streamA')
      })

      // User switches to doc B mid-stream. docRef.current updates to flow-B via
      // the post-render effect.
      act(() => {
        rerender({doc: docB})
      })

      // Stream for threadA (flow-A) completes after the switch. The save must
      // target flow-A, not the now-current flow-B.
      act(() => {
        capturedHandler!.onDone(5, 10, 'streamA')
      })

      expect(chatApi.saveConversation).toHaveBeenCalledWith(
        'flow-A',
        'block1',
        expect.arrayContaining([expect.objectContaining({role: 'assistant', content: 'hello from A'})]),
      )
      // Must NOT have saved under the switched-to doc.
      expect(chatApi.saveConversation).not.toHaveBeenCalledWith(
        'flow-B',
        expect.anything(),
        expect.anything(),
      )
    })
  })
})
