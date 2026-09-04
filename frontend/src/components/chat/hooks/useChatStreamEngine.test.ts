import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'

// Bind to the REAL StreamHandler contract. This test used to declare a private
// copy of the shape, whose onFixDecision took a 5th `items` argument that the
// production dispatcher never actually passed — so the batch-decision test below
// drove a contract no code implemented and stayed green over a live bug.
// Importing the real type turns any future drift into a typecheck failure.
import type {StreamHandler} from '@/hooks/useStreamingMessage'

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
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
  },
  flowApi: {
    loadFlowFromPath: vi.fn().mockResolvedValue(null),
  },
  analysisApi: {
    analyzeFlowById: vi.fn().mockResolvedValue({flowId: 'flow-1', findings: []}),
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
            threadB: {
              streamId: 'streamB',
              messageId: 'mB',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamB',
              messageId: 'mB',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamB',
              messageId: 'mB',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
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
      expect(chatApi.saveConversation).not.toHaveBeenCalledWith('flow-B', expect.anything(), expect.anything())
    })
  })

  describe('apply_fix proposal flow', () => {
    function setupFixStream() {
      const hook = renderEngine()
      act(() => {
        hook.result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 't',
              createdAt: new Date().toISOString(),
              contextBlockId: null,
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          streams: {
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
          },
        })
      })
      return hook
    }

    it('onFixProposal shows the pending approval card on the thread slot', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [
              {
                ruleId: 'unhandled-error',
                fixType: 'wrap-error-handler',
                blockLabel: 'Call API',
                line: 3,
                summary: 'wrap lines 3-3',
              },
            ],
          },
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA
      expect(slot.fixProposals).toHaveLength(1)
      expect(slot.fixProposals[0]).toMatchObject({
        proposalId: 'p1',
        ruleId: 'unhandled-error',
        fixType: 'wrap-error-handler',
        blockLabel: 'Call API',
        line: 3,
        summary: 'wrap lines 3-3',
        status: 'pending',
      })
      expect(slot.fixProposals[0].items).toHaveLength(1)
      expect(slot.fixProposals[0].items[0]).toMatchObject({ruleId: 'unhandled-error', status: 'pending'})
    })

    // U2: sequential proposals STACK — the model can propose a second fix
    // after the first resolves; the earlier outcome must survive.
    it('a second proposal appends instead of replacing the first card', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [{ruleId: 'r1', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('p1', 'applied', undefined, 'streamA')
      })
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p2',
            items: [{ruleId: 'r2', fixType: 'f2', blockLabel: 'b2', line: 5, summary: 's2'}],
          },
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA
      expect(slot.fixProposals).toHaveLength(2)
      expect(slot.fixProposals[0].status).toBe('applied')
      expect(slot.fixProposals[0].proposalId).toBe('p1')
      expect(slot.fixProposals[1].status).toBe('pending')
      expect(slot.fixProposals[1].proposalId).toBe('p2')
    })

    // Replaying a proposalId already on the slot is a no-op (journal replay
    // idempotence).
    it('replaying the same proposalId does not duplicate the card', () => {
      setupFixStream()
      const payload = {
        kind: 'fix-proposal' as const,
        proposalId: 'p1',
        items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
      }
      act(() => {
        capturedHandler!.onFixProposal!(payload, 'streamA')
      })
      act(() => {
        capturedHandler!.onFixProposal!(payload, 'streamA')
      })
      expect(useChatStore.getState().streams.threadA.fixProposals).toHaveLength(1)
    })

    // Batch decisions patch per-item statuses.
    it('batch decision items patch per-item statuses', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'batch-1',
            items: [
              {ruleId: 'r1', fixType: 'f', blockLabel: 'b1', line: 1, summary: 's'},
              {ruleId: 'r2', fixType: 'f', blockLabel: 'b2', line: 2, summary: 's'},
            ],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('batch-1', 'applied-unresolved', 'review', 'streamA', [
          {ruleId: 'r1', status: 'applied'},
          {ruleId: 'r2', status: 'applied-unresolved', message: 'still appears'},
        ])
      })
      const card = useChatStore.getState().streams.threadA.fixProposals[0]
      expect(card.status).toBe('applied-unresolved')
      expect(card.items[0].status).toBe('applied')
      expect(card.items[1].status).toBe('applied-unresolved')
      expect(card.items[1].message).toBe('still appears')
    })

    // R1 frontend half: a resume journal rebuilds the slot's agentic state
    // wholesale — reconnecting mid-approval restores the pending card instead
    // of orphaning it into a 60s timeout.
    it('onResumeState rebuilds tool trail and pending proposals from the journal', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onResumeState!(
          [
            {
              kind: 'fix-proposal',
              proposalId: 'p1',
              items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
            },
            {
              kind: 'tool-result',
              name: 'search_flow',
              label: 'Searching flow',
              ok: true,
              durationMs: 4,
              summary: '2 matches',
            },
            {kind: 'fix-decision', proposalId: 'p1', status: 'applied', message: 'verified'},
          ],
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA
      expect(slot.toolCalls).toEqual([
        {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 4, summary: '2 matches'},
      ])
      expect(slot.fixProposals).toHaveLength(1)
      expect(slot.fixProposals[0].status).toBe('applied')

      // Idempotent: replaying the same journal must not duplicate state.
      act(() => {
        capturedHandler!.onResumeState!(
          [
            {
              kind: 'fix-proposal',
              proposalId: 'p1',
              items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
            },
          ],
          'streamA',
        )
      })
      expect(useChatStore.getState().streams.threadA.toolCalls).toHaveLength(0)
      expect(useChatStore.getState().streams.threadA.fixProposals).toHaveLength(1)
    })

    it('onFixDecision applied patches the card and re-analyzes the flow', async () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('p1', 'applied', undefined, 'streamA')
      })
      expect(useChatStore.getState().streams.threadA.fixProposals[0]?.status).toBe('applied')
      // The refresh fires asynchronously; flush microtasks.
      await act(async () => {
        await Promise.resolve()
      })
      const {analysisApi} = await import('@/api')
      expect(analysisApi.analyzeFlowById).toHaveBeenCalledWith('flow-1')
    })

    it('respondFixProposal posts the decision only while pending', async () => {
      const {result} = setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', true)
      })
      expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'p1', true, undefined)
      expect(useChatStore.getState().streams.threadA.fixProposals[0]?.status).toBe('applying')

      // Second click while no longer pending must not re-send.
      await act(async () => {
        result.current.respondFixProposal('threadA', false)
      })
      expect(chatApi.respondFixDecision).toHaveBeenCalledTimes(1)
    })

    it('respondFixProposal failure marks the card error', async () => {
      const {chatApi: api} = await import('@/api')
      vi.mocked(api.respondFixDecision).mockRejectedValueOnce(new Error('network'))
      const {result} = setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', true)
      })
      expect(useChatStore.getState().streams.threadA.fixProposals[0]?.status).toBe('error')
    })
  })

  // The transparency trail: finished tool executions (tool_result events)
  // accumulate on the live slot and are pinned onto the committed assistant
  // message — together with the fix-proposal outcome snapshot — so they
  // persist with the saved conversation instead of dying with the slot.
  describe('tool trail persistence', () => {
    function setupToolStream() {
      const hook = renderEngine()
      act(() => {
        hook.result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          threads: [
            {
              id: 'threadA',
              flowId: 'flow-1',
              title: 't',
              createdAt: new Date().toISOString(),
              contextBlockId: null,
              selectedSourceFiles: [],
              tokensIn: 0,
              tokensOut: 0,
            },
          ],
          conversations: new Map([['threadA', []]]),
          streams: {
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              fixProposals: [],
            },
          },
        })
      })
      return hook
    }

    it('onToolResult accumulates records on the slot', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onToolResult!(
          {
            kind: 'tool-result',
            name: 'search_flow',
            label: 'Searching flow',
            ok: true,
            durationMs: 7,
            summary: '3 matches',
          },
          'streamA',
        )
        capturedHandler!.onToolResult!(
          {
            kind: 'tool-result',
            name: 'read_doc',
            label: 'Reading document',
            ok: false,
            durationMs: 1,
            summary: 'error: no doc',
          },
          'streamA',
        )
      })
      expect(useChatStore.getState().streams.threadA.toolCalls).toEqual([
        {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 7, summary: '3 matches'},
        {name: 'read_doc', label: 'Reading document', ok: false, durationMs: 1, summary: 'error: no doc'},
      ])
    })

    // U1: answer text resuming after a tool phase clears the stale
    // "Using tool…" pulse instead of keeping it lit under the final answer.
    it('a text chunk clears a stale toolStatus', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA')
        useChatStore.setState({
          streams: {
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: 'Searching flow',
              toolCalls: [],
              fixProposals: [],
            },
          },
        })
      })
      act(() => {
        capturedHandler!.onChunk('the answer', 'streamA')
      })
      expect(useChatStore.getState().streams.threadA.toolStatus).toBeNull()
      expect(useChatStore.getState().streams.threadA.text).toBe('the answer')
    })

    // U3: an errored turn keeps its tool trail — 4 tools ran before the
    // failure, and the error bubble should show them.
    it('onError pins the accumulated tool trail onto the error message', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onToolResult!(
          {
            kind: 'tool-result',
            name: 'search_flow',
            label: 'Searching flow',
            ok: true,
            durationMs: 3,
            summary: '2 matches',
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onError('the AI provider is temporarily unavailable', 'streamA')
      })
      const msgs = useChatStore.getState().conversations.get('threadA')!
      expect(msgs).toHaveLength(1)
      expect(msgs[0].finishReason).toBe('error')
      expect(msgs[0].toolCalls).toEqual([
        {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 3, summary: '2 matches'},
      ])
    })

    it('onToolResult ignores an unknown streamId', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onToolResult!(
          {kind: 'tool-result', name: 'x', label: 'x', ok: true, durationMs: 0, summary: ''},
          'ghost',
        )
      })
      expect(useChatStore.getState().streams.threadA.toolCalls).toHaveLength(0)
    })

    it('done pins toolCalls and the fix outcome snapshot onto the committed message', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onChunk('Fixed it', 'streamA')
        capturedHandler!.onToolResult!(
          {
            kind: 'tool-result',
            name: 'list_findings',
            label: 'Listing findings',
            ok: true,
            durationMs: 3,
            summary: '1 finding',
          },
          'streamA',
        )
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [
              {
                ruleId: 'unhandled-error',
                fixType: 'wrap-error-handler',
                blockLabel: 'Call API',
                line: 3,
                summary: 'wrap',
              },
            ],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('p1', 'applied', 'verified gone', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(9, 4, 'streamA')
      })

      const msgs = useChatStore.getState().conversations.get('threadA')!
      expect(msgs).toHaveLength(1)
      const committed = msgs[0]
      expect(committed.content).toBe('Fixed it')
      expect(committed.toolCalls).toEqual([
        {name: 'list_findings', label: 'Listing findings', ok: true, durationMs: 3, summary: '1 finding'},
      ])
      expect(committed.fixProposals).toHaveLength(1)
      expect(committed.fixProposals![0]).toMatchObject({
        proposalId: 'p1',
        ruleId: 'unhandled-error',
        fixType: 'wrap-error-handler',
        blockLabel: 'Call API',
        line: 3,
        status: 'applied',
        message: 'verified gone',
      })
      expect(committed.fixProposals![0].items).toHaveLength(1)
      expect(committed.fixProposals![0].items![0].status).toBe('applied')
      // The trail is persisted with the conversation save.
      expect(chatApi.saveConversation).toHaveBeenCalledWith(
        'flow-1',
        'flow',
        expect.arrayContaining([
          expect.objectContaining({toolCalls: committed.toolCalls, fixProposals: committed.fixProposals}),
        ]),
      )
      // Slot is gone after done — the message carries the record now.
      expect(useChatStore.getState().streams.threadA).toBeUndefined()
    })

    // F10: commit flags the thread as agentic so the tab shows badges.
    it('done flags the thread usedTools/appliedFixes for tab badges', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onChunk('Fixed it', 'streamA')
        capturedHandler!.onToolResult!(
          {
            kind: 'tool-result',
            name: 'list_findings',
            label: 'Listing findings',
            ok: true,
            durationMs: 3,
            summary: '1 finding',
          },
          'streamA',
        )
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            proposalId: 'p1',
            items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('p1', 'applied', undefined, 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(9, 4, 'streamA')
      })
      const thread = useChatStore.getState().threads.find(t => t.id === 'threadA')!
      expect(thread.usedTools).toBe(true)
      expect(thread.appliedFixes).toBe(true)
    })

    it('done without tools commits a message with no trail fields', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onChunk('plain answer', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(2, 2, 'streamA')
      })
      const committed = useChatStore.getState().conversations.get('threadA')![0]
      expect(committed.toolCalls).toBeUndefined()
      expect(committed.fixProposals).toBeUndefined()
    })
  })
})
