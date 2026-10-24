import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'

import type {StreamHandler} from '@/hooks/useStreamingMessage'

let capturedHandler: StreamHandler | null = null
let capturedResume: {streamId: string; opts: {mode?: 'delta' | 'full'; onGone?: () => void} | undefined} | null = null
let registerStreamImpl: () => Promise<boolean> = async () => true

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: (handler: StreamHandler) => {
    capturedHandler = handler
    return {
      registerStream: () => registerStreamImpl(),
      confirmStream: vi.fn(),
      cancel: vi.fn(),
      teardownStream: vi.fn(),
      resumeInto: (streamId: string, opts?: {mode?: 'delta' | 'full'; onGone?: () => void}) => {
        capturedResume = {streamId, opts}
      },
    }
  },
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    streamMessage: vi.fn(),
    cancelStream: vi.fn(),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
    respondApprovalDecision: vi.fn().mockResolvedValue(undefined),
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
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument} from '@/types'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

function renderEngine() {
  return renderHook(() => useChatStreamEngine())
}

const LOADED = {state: 'loaded' as const, token: 1}

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
  capturedResume = null
  registerStreamImpl = async () => true
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
        result.current.beginAcc('streamA', 'threadA', META)
        result.current.beginAcc('streamB', 'threadB', META)
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
              approvals: [],
            },
            threadB: {
              streamId: 'streamB',
              messageId: 'mB',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              approvals: [],
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
      expect(streams.threadA!.text).toBe('Hello from A more A')
      expect(streams.threadB!.text).toBe('Hello from B')
    })

    it('ignores chunks for an unknown streamId', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
            },
          },
        })
      })
      act(() => {
        capturedHandler!.onChunk('ghost', 'unknown-stream')
      })
      expect(useChatStore.getState().streams.threadA!.text).toBe('')
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
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
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
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
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
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
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
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
            },
          },
        })
      })
      act(() => {
        capturedHandler!.onError('provider down', 'streamA')
      })
      const msgs = useChatStore.getState().conversations.get('threadA')
      expect(msgs).toHaveLength(1)
      expect(msgs![0]!.finishReason).toBe('error')
      expect(msgs![0]!.content).toContain('provider down')
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
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
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
        result.current.commitAssistantMessage({threadId: 'threadA', ...META}, 'mA', 'hello world', {
          finishReason: 'interrupted',
        })
      })
      const msgs = useChatStore.getState().conversations.get('threadA')
      expect(msgs).toHaveLength(1)
      expect(msgs![0]!.content).toBe('hello world')
      expect(msgs![0]!.finishReason).toBe('interrupted')
    })
  })

  describe('commitAssistantMessage doc-switch safety', () => {
    it('saves under the thread flowId, not the switched-to doc', () => {
      const docA = {id: 'flow-A', name: 'A', subflows: []} as unknown as FlowDocument
      const docB = {id: 'flow-B', name: 'B', subflows: []} as unknown as FlowDocument

      useFlowStore.setState({document: docA})
      const {result} = renderHook(() => useChatStreamEngine())

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
          historyStatus: {threadA: LOADED},
          streams: {
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              approvals: [],
            },
          },
        })
        result.current.beginAcc('streamA', 'threadA', META)
        capturedHandler!.onChunk('hello from A', 'streamA')
      })

      act(() => {
        useFlowStore.setState({document: docB})
      })

      act(() => {
        capturedHandler!.onDone(5, 10, 'streamA')
      })

      expect(chatApi.saveConversation).toHaveBeenCalledWith(
        'flow-A',
        'threadA',
        expect.arrayContaining([expect.objectContaining({role: 'assistant', content: 'hello from A'})]),
      )
      expect(chatApi.saveConversation).not.toHaveBeenCalledWith('flow-B', expect.anything(), expect.anything())
    })
  })

  describe('"Save conversation history" off', () => {
    it('does not store the finished turn', () => {
      const prevSettings = useSettingsStore.getState().settings
      useSettingsStore.setState({settings: {...prevSettings, ai: {...prevSettings.ai, saveConversationHistory: false}}})
      try {
        const {result} = renderHook(() => useChatStreamEngine())
        act(() => {
          useChatStore.setState({
            threads: [
              {
                id: 'threadA',
                flowId: 'flow-A',
                title: 'T',
                createdAt: '2024',
                contextBlockId: '',
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
                approvals: [],
              },
            },
          })
          result.current.beginAcc('streamA', 'threadA', META)
          capturedHandler!.onChunk('not kept', 'streamA')
        })
        act(() => {
          capturedHandler!.onDone(5, 10, 'streamA')
        })

        expect(useChatStore.getState().conversations.get('threadA')).toEqual(
          expect.arrayContaining([expect.objectContaining({role: 'assistant', content: 'not kept'})]),
        )
        expect(chatApi.saveConversation).not.toHaveBeenCalled()
      } finally {
        useSettingsStore.setState({settings: prevSettings})
      }
    })
  })

  describe('apply_fix proposal flow', () => {
    function setupFixStream() {
      const hook = renderEngine()
      act(() => {
        hook.result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [
              {
                id: 'unhandled-error',
                label: 'Call API',
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
      const slot = useChatStore.getState().streams.threadA!
      expect(slot.approvals).toHaveLength(1)
      expect(slot.approvals[0]!).toMatchObject({
        proposalId: 'p1',
        ruleId: 'unhandled-error',
        fixType: 'wrap-error-handler',
        blockLabel: 'Call API',
        line: 3,
        summary: 'wrap lines 3-3',
        status: 'pending',
      })
      expect(slot.approvals[0]!.items).toHaveLength(1)
      expect(slot.approvals[0]!.items[0]!).toMatchObject({ruleId: 'unhandled-error', status: 'pending'})
    })

    it('a second proposal appends instead of replacing the first card', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [{id: 'r1', label: 'b', ruleId: 'r1', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p2',
            items: [{id: 'r2', label: 'b2', ruleId: 'r2', fixType: 'f2', blockLabel: 'b2', line: 5, summary: 's2'}],
          },
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA!
      expect(slot.approvals).toHaveLength(2)
      expect(slot.approvals[0]!.status).toBe('applied')
      expect(slot.approvals[0]!.proposalId).toBe('p1')
      expect(slot.approvals[1]!.status).toBe('pending')
      expect(slot.approvals[1]!.proposalId).toBe('p2')
    })

    it('replaying the same proposalId does not duplicate the card', () => {
      setupFixStream()
      const payload = {
        kind: 'fix-proposal' as const,
        source: 'fix' as const,
        tool: 'apply_fix',
        previewKind: 'fix-patch',
        proposalId: 'p1',
        items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
      }
      act(() => {
        capturedHandler!.onFixProposal!(payload, 'streamA')
      })
      act(() => {
        capturedHandler!.onFixProposal!(payload, 'streamA')
      })
      expect(useChatStore.getState().streams.threadA!.approvals).toHaveLength(1)
    })

    it('batch decision items patch per-item statuses', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'batch-1',
            items: [
              {id: 'r1', label: 'b1', ruleId: 'r1', fixType: 'f', blockLabel: 'b1', line: 1, summary: 's'},
              {id: 'r2', label: 'b2', ruleId: 'r2', fixType: 'f', blockLabel: 'b2', line: 2, summary: 's'},
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
      const card = useChatStore.getState().streams.threadA!.approvals[0]!
      expect(card.status).toBe('applied-unresolved')
      expect(card.items[0]!.status).toBe('applied')
      expect(card.items[1]!.status).toBe('applied-unresolved')
      expect(card.items[1]!.message).toBe('still appears')
    })

    it('onResumeState rebuilds tool trail and pending proposals from the journal', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onResumeState!(
          [
            {
              kind: 'fix-proposal',
              source: 'fix',
              tool: 'apply_fix',
              previewKind: 'fix-patch',
              proposalId: 'p1',
              items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
            },
            {
              kind: 'tool-result',
              name: 'search_flow',
              label: 'Searching flow',
              ok: true,
              durationMs: 4,
              summary: '2 matches',
            },
            {kind: 'fix-decision', source: 'fix', proposalId: 'p1', status: 'applied', message: 'verified'},
          ],
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA!
      expect(slot.toolCalls).toEqual([
        {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 4, summary: '2 matches'},
      ])
      expect(slot.approvals).toHaveLength(1)
      expect(slot.approvals[0]!.status).toBe('applied')

      act(() => {
        capturedHandler!.onResumeState!(
          [
            {
              kind: 'fix-proposal',
              source: 'fix',
              tool: 'apply_fix',
              previewKind: 'fix-patch',
              proposalId: 'p1',
              items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
            },
          ],
          'streamA',
        )
      })
      expect(useChatStore.getState().streams.threadA!.toolCalls).toHaveLength(0)
      expect(useChatStore.getState().streams.threadA!.approvals).toHaveLength(1)
    })

    it('onFixDecision applied patches the card and re-analyzes the flow', async () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('p1', 'applied', undefined, 'streamA')
      })
      expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('applied')
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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', true)
      })
      expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'p1', true, undefined)
      expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('applying')

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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', true)
      })
      expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('error')
    })

    it('keeps tool, previewKind and requiredScope on the card; an older server defaults to fix-patch', () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            proposalId: 'p1',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            requiredScope: 'write',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            proposalId: 'p2',
            tool: '',
            previewKind: 'fix-patch',
            items: [{id: 'r2', label: 'b', ruleId: 'r2', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      const slot = useChatStore.getState().streams.threadA!
      expect(slot.approvals[0]).toMatchObject({
        source: 'fix',
        tool: 'apply_fix',
        previewKind: 'fix-patch',
        requiredScope: 'write',
      })
      expect(slot.approvals[1]).toMatchObject({source: 'fix', tool: '', previewKind: 'fix-patch'})
      expect(slot.approvals[1]).not.toHaveProperty('requiredScope')
    })

    it('routes the decision to /api/chat/approval-decision for an approval_request and to /api/chat/fix-decision for a fix_proposal', async () => {
      const {result} = setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'approval',
            proposalId: 'a1',
            tool: 'rename_variable',
            previewKind: 'something-new',
            items: [
              {
                id: 'v1',
                label: 'Variable %A%',
                ruleId: '',
                fixType: '',
                blockLabel: '',
                line: 0,
                summary: 'to %Alpha%',
              },
            ],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', true, 'a1')
      })
      expect(chatApi.respondApprovalDecision).toHaveBeenCalledWith('streamA', 'a1', true, undefined)
      expect(chatApi.respondFixDecision).not.toHaveBeenCalled()
      expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('applying')

      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            proposalId: 'p1',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      await act(async () => {
        result.current.respondFixProposal('threadA', false, 'p1')
      })
      expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'p1', false, undefined)
      expect(chatApi.respondApprovalDecision).toHaveBeenCalledTimes(1)
    })

    // R-frontend-1: Approve on a read-only flow returned before doing anything: no decision, no patch, and the card
    // sat pending until the server timed it out and told the model the user never answered.
    it('turns Approve on a read-only flow into a decline the card shows, for fix and approval cards alike', async () => {
      useFlowStore.setState({readOnly: true})
      try {
        const {result} = setupFixStream()
        for (const [source, proposalId] of [
          ['fix', 'p1'],
          ['approval', 'a1'],
        ] as const) {
          act(() => {
            capturedHandler!.onFixProposal!(
              {
                kind: 'fix-proposal',
                source,
                proposalId,
                tool: source === 'fix' ? 'apply_fix' : 'rename_variable',
                previewKind: 'fix-patch',
                items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
              },
              'streamA',
            )
          })
          await act(async () => {
            result.current.respondFixProposal('threadA', true, proposalId)
          })
          const card = useChatStore.getState().streams.threadA!.approvals.find(c => c.proposalId === proposalId)
          expect(card?.status).toBe('declined')
          expect(card?.message).toMatch(/read-only/i)
        }
        expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'p1', false, undefined)
        expect(chatApi.respondApprovalDecision).toHaveBeenCalledWith('streamA', 'a1', false, undefined)
        expect(chatApi.respondFixDecision).not.toHaveBeenCalledWith('streamA', 'p1', true, undefined)
        // The server's echo of the decline carries no message: the reason stays on the card.
        act(() => {
          capturedHandler!.onFixDecision!('p1', 'declined', undefined, 'streamA')
        })
        expect(useChatStore.getState().streams.threadA!.approvals.find(c => c.proposalId === 'p1')?.message).toMatch(
          /read-only/i,
        )

        // Decline still works as a decline.
        act(() => {
          capturedHandler!.onFixProposal!(
            {
              kind: 'fix-proposal',
              source: 'fix',
              proposalId: 'p2',
              tool: 'apply_fix',
              previewKind: 'fix-patch',
              items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
            },
            'streamA',
          )
        })
        await act(async () => {
          result.current.respondFixProposal('threadA', false, 'p2')
        })
        expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'p2', false, undefined)
        expect(useChatStore.getState().streams.threadA!.approvals.find(c => c.proposalId === 'p2')?.status).toBe(
          'declined',
        )
      } finally {
        useFlowStore.setState({readOnly: false})
      }
    })

    it('shows a cancelled decision as cancelled, with the server message', async () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            proposalId: 'p1',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('p1', 'cancelled', 'the server is restarting', 'streamA')
      })
      const card = useChatStore.getState().streams.threadA!.approvals[0]!
      expect(card.status).toBe('cancelled')
      expect(card.message).toBe('the server is restarting')
      await act(async () => {
        await Promise.resolve()
      })
      const {analysisApi} = await import('@/api')
      expect(analysisApi.analyzeFlowById).not.toHaveBeenCalled()
    })

    it("patches a generic request's items by id", () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'approval',
            proposalId: 'a1',
            tool: 'rename_variable',
            previewKind: 'something-new',
            items: [
              {id: 'v1', label: 'A', ruleId: '', fixType: '', blockLabel: '', line: 0, summary: 's'},
              {id: 'v2', label: 'B', ruleId: '', fixType: '', blockLabel: '', line: 0, summary: 's'},
            ],
          },
          'streamA',
        )
      })
      act(() => {
        capturedHandler!.onFixDecision!('a1', 'applied', undefined, 'streamA', [
          {id: 'v2', status: 'error', message: 'skipped'},
        ])
      })
      const card = useChatStore.getState().streams.threadA!.approvals[0]!
      expect(card.status).toBe('applied')
      expect(card.items[0]!.status).toBe('pending')
      expect(card.items[1]).toMatchObject({status: 'error', message: 'skipped'})
    })

    it('matches a batch decision to rows sharing a rule by id, else by position', () => {
      setupFixStream()
      const sameRule = (id: string, label: string) => ({
        id,
        label,
        ruleId: 'unhandled-error',
        fixType: 'f',
        blockLabel: label,
        line: 1,
        summary: 's',
      })
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fixes',
            previewKind: 'fix-patch',
            proposalId: 'batch-2',
            items: [sameRule('unhandled-error', 'Call A'), sameRule('unhandled-error', 'Call B')],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('batch-2', 'applied-unresolved', undefined, 'streamA', [
          {ruleId: 'unhandled-error', status: 'applied'},
          {ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
        ])
      })
      const legacy = useChatStore.getState().streams.threadA!.approvals[0]!
      expect(legacy.items[0]).toMatchObject({label: 'Call A', status: 'applied'})
      expect(legacy.items[1]).toMatchObject({label: 'Call B', status: 'error', message: 'block changed'})

      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fixes',
            previewKind: 'fix-patch',
            proposalId: 'batch-3',
            items: [sameRule('b1:unhandled-error', 'Call A'), sameRule('b2:unhandled-error', 'Call B')],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('batch-3', 'applied-unresolved', undefined, 'streamA', [
          {id: 'b2:unhandled-error', ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
          {id: 'b1:unhandled-error', ruleId: 'unhandled-error', status: 'applied'},
        ])
      })
      const stamped = useChatStore.getState().streams.threadA!.approvals[1]!
      expect(stamped.items[0]).toMatchObject({label: 'Call A', status: 'applied'})
      expect(stamped.items[1]).toMatchObject({label: 'Call B', status: 'error', message: 'block changed'})
    })

    it('replays a journaled batch decision onto rows sharing a rule by position', () => {
      setupFixStream()
      const sameRule = (label: string) => ({
        id: 'unhandled-error',
        label,
        ruleId: 'unhandled-error',
        fixType: 'f',
        blockLabel: label,
        line: 1,
        summary: 's',
      })
      act(() => {
        capturedHandler!.onResumeState!(
          [
            {
              kind: 'fix-proposal',
              source: 'fix',
              tool: 'apply_fixes',
              previewKind: 'fix-patch',
              proposalId: 'batch-2',
              items: [sameRule('Call A'), sameRule('Call B')],
            },
            {
              kind: 'fix-decision',
              source: 'fix',
              proposalId: 'batch-2',
              status: 'applied-unresolved',
              items: [
                {ruleId: 'unhandled-error', status: 'applied'},
                {ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
              ],
            },
          ],
          'streamA',
        )
      })
      const card = useChatStore.getState().streams.threadA!.approvals[0]!
      expect(card.status).toBe('applied-unresolved')
      expect(card.items[0]).toMatchObject({label: 'Call A', status: 'applied'})
      expect(card.items[1]).toMatchObject({label: 'Call B', status: 'error', message: 'block changed'})
    })

    it('does not refresh the flow when an approval of another kind is applied', async () => {
      setupFixStream()
      act(() => {
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'approval',
            proposalId: 'a1',
            tool: 'rename_variable',
            previewKind: 'something-new',
            items: [{id: 'v1', label: 'A', ruleId: '', fixType: '', blockLabel: '', line: 0, summary: 's'}],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('a1', 'applied', undefined, 'streamA')
      })
      expect(useChatStore.getState().streams.threadA!.approvals[0]).toMatchObject({
        status: 'applied',
        items: [{status: 'applied'}],
      })
      await act(async () => {
        await Promise.resolve()
      })
      const {analysisApi} = await import('@/api')
      expect(analysisApi.analyzeFlowById).not.toHaveBeenCalled()
    })

    it('falls back to /api/chat/fix-decision once when /api/chat/approval-decision is 404', async () => {
      const {result} = setupFixStream()
      const {ApiError} = await import('@/api')
      const request = {
        kind: 'fix-proposal' as const,
        source: 'approval' as const,
        tool: 'rename_variable',
        previewKind: 'something-new',
        items: [{id: 'v1', label: 'A', ruleId: '', fixType: '', blockLabel: '', line: 0, summary: 's'}],
      }
      act(() => {
        capturedHandler!.onFixProposal!({...request, proposalId: 'a1'}, 'streamA')
        capturedHandler!.onFixProposal!({...request, proposalId: 'a2'}, 'streamA')
      })
      vi.mocked(chatApi.respondApprovalDecision).mockRejectedValueOnce(
        Object.assign(new ApiError('not found', 404), {status: 404}),
      )
      await act(async () => {
        result.current.respondFixProposal('threadA', true, 'a1', [0])
      })
      expect(chatApi.respondApprovalDecision).toHaveBeenCalledWith('streamA', 'a1', true, [0])
      expect(chatApi.respondFixDecision).toHaveBeenCalledTimes(1)
      expect(chatApi.respondFixDecision).toHaveBeenCalledWith('streamA', 'a1', true, [0])
      expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('applying')

      vi.mocked(chatApi.respondApprovalDecision).mockRejectedValueOnce(
        Object.assign(new ApiError('boom', 500), {status: 500}),
      )
      await act(async () => {
        result.current.respondFixProposal('threadA', false, 'a2')
      })
      expect(chatApi.respondFixDecision).toHaveBeenCalledTimes(1)
      expect(useChatStore.getState().streams.threadA!.approvals[1]?.status).toBe('error')
    })
  })

  describe('tool trail persistence', () => {
    function setupToolStream() {
      const hook = renderEngine()
      act(() => {
        hook.result.current.beginAcc('streamA', 'threadA', META)
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
          historyStatus: {threadA: LOADED},
          streams: {
            threadA: {
              streamId: 'streamA',
              messageId: 'mA',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              approvals: [],
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
      expect(useChatStore.getState().streams.threadA!.toolCalls).toEqual([
        {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 7, summary: '3 matches'},
        {name: 'read_doc', label: 'Reading document', ok: false, durationMs: 1, summary: 'error: no doc'},
      ])
    })

    it('a text chunk clears a stale toolStatus', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA', META)
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
              approvals: [],
            },
          },
        })
      })
      act(() => {
        capturedHandler!.onChunk('the answer', 'streamA')
      })
      expect(useChatStore.getState().streams.threadA!.toolStatus).toBeNull()
      expect(useChatStore.getState().streams.threadA!.text).toBe('the answer')
    })

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
      expect(msgs[0]!.finishReason).toBe('error')
      expect(msgs[0]!.toolCalls).toEqual([
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
      expect(useChatStore.getState().streams.threadA!.toolCalls).toHaveLength(0)
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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [
              {
                id: 'unhandled-error',
                label: 'Call API',
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
      const committed = msgs[0]!
      expect(committed.content).toBe('Fixed it')
      expect(committed.toolCalls).toEqual([
        {name: 'list_findings', label: 'Listing findings', ok: true, durationMs: 3, summary: '1 finding'},
      ])
      expect(committed.fixProposals).toHaveLength(1)
      expect(committed.fixProposals![0]!).toMatchObject({
        proposalId: 'p1',
        ruleId: 'unhandled-error',
        fixType: 'wrap-error-handler',
        blockLabel: 'Call API',
        line: 3,
        status: 'applied',
        message: 'verified gone',
      })
      expect(committed.fixProposals![0]!.items).toHaveLength(1)
      expect(committed.fixProposals![0]!.items![0]!.status).toBe('applied')
      expect(chatApi.saveConversation).toHaveBeenCalledWith(
        'flow-1',
        'threadA',
        expect.arrayContaining([
          expect.objectContaining({toolCalls: committed.toolCalls, fixProposals: committed.fixProposals}),
        ]),
      )
      expect(useChatStore.getState().streams.threadA).toBeUndefined()
    })

    it('done writes tool and previewKind onto the persisted fix snapshot', () => {
      setupToolStream()
      act(() => {
        capturedHandler!.onChunk('Done', 'streamA')
        capturedHandler!.onFixProposal!(
          {
            kind: 'fix-proposal',
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            requiredScope: 'write',
            proposalId: 'p1',
            items: [
              {
                id: 'b1:r',
                label: 'Call API',
                ruleId: 'r',
                fixType: 'f',
                blockLabel: 'Call API',
                line: 3,
                summary: 'wrap',
              },
            ],
          },
          'streamA',
        )
        capturedHandler!.onFixDecision!('p1', 'applied', 'verified gone', 'streamA')
        capturedHandler!.onDone(9, 4, 'streamA')
      })
      const committed = useChatStore.getState().conversations.get('threadA')![0]!
      expect(committed.fixProposals).toEqual([
        {
          proposalId: 'p1',
          tool: 'apply_fix',
          previewKind: 'fix-patch',
          ruleId: 'r',
          fixType: 'f',
          blockLabel: 'Call API',
          line: 3,
          summary: 'wrap',
          status: 'applied',
          message: 'verified gone',
          items: [
            {
              ruleId: 'r',
              fixType: 'f',
              blockLabel: 'Call API',
              line: 3,
              summary: 'wrap',
              status: 'applied',
              message: 'verified gone',
            },
          ],
        },
      ])
    })

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
            source: 'fix',
            tool: 'apply_fix',
            previewKind: 'fix-patch',
            proposalId: 'p1',
            items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
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
      const committed = useChatStore.getState().conversations.get('threadA')![0]!
      expect(committed.toolCalls).toBeUndefined()
      expect(committed.fixProposals).toBeUndefined()
    })
  })

  describe('served model on done', () => {

    function seedThread(threadId: string, streamId: string, provider: string) {
      const st = useChatStore.getState()
      useChatStore.setState({
        threads: [
          ...st.threads,
          {
            id: threadId,
            flowId: 'flow-1',
            title: 't',
            createdAt: '2024',
            contextBlockId: null,
            selectedSourceFiles: [],
            tokensIn: 0,
            tokensOut: 0,
          },
        ],
        conversations: new Map([...st.conversations, [threadId, []]]),
        streams: {
          ...st.streams,
          [threadId]: {
            streamId,
            messageId: `m-${streamId}`,
            text: '',
            isThinking: true,
            tokens: 0,
            toolStatus: null,
            toolCalls: [],
            approvals: [],
          },
        },
      })
      return {provider: provider as never, model: 'my-deployment'}
    }

    it('pins the served model onto the committed message', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'azure-foundry'))
        capturedHandler!.onChunk('answer', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(9, 4, 'streamA', {servedModel: 'gpt-4o-2024-08-06'})
      })
      const committed = useChatStore.getState().conversations.get('threadA')![0]!
      expect(committed).toMatchObject({
        content: 'answer',
        model: 'my-deployment',
        servedModel: 'gpt-4o-2024-08-06',
        tokensIn: 4,
        tokensOut: 9,
      })
    })

    it('leaves the fields off a message whose done carried none', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'azure-foundry'))
        capturedHandler!.onChunk('answer', 'streamA')
      })
      act(() => {
        capturedHandler!.onDone(2, 2, 'streamA')
      })
      const committed = useChatStore.getState().conversations.get('threadA')![0]!
      expect('servedModel' in committed).toBe(false)
      expect('inputCostPerM' in committed).toBe(false)
      expect('outputCostPerM' in committed).toBe(false)
    })

    it('refetches the provider list once per thread when an alias provider names the served model', () => {
      const {result} = renderEngine()
      const epoch0 = useChatStore.getState().providerEpoch
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'azure-foundry'))
      })
      act(() => {
        capturedHandler!.onDone(1, 1, 'streamA', {servedModel: 'gpt-4o'})
      })
      expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)

      act(() => {
        useChatStore.setState({
          streams: {
            threadA: {
              streamId: 'streamA2',
              messageId: 'mA2',
              text: '',
              isThinking: true,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
              approvals: [],
            },
          },
        })
        result.current.beginAcc('streamA2', 'threadA', {provider: 'azure-foundry' as never, model: 'my-deployment'})
      })
      act(() => {
        capturedHandler!.onDone(1, 1, 'streamA2', {servedModel: 'gpt-4o'})
      })
      expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)

      act(() => {
        result.current.beginAcc('streamB', 'threadB', seedThread('threadB', 'streamB', 'azure-foundry'))
      })
      act(() => {
        capturedHandler!.onDone(1, 1, 'streamB', {servedModel: 'gpt-4o'})
      })
      expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 2)
    })

    it('does not refetch for a provider without aliases, nor for a done without a served model', () => {
      const {result} = renderEngine()
      const epoch0 = useChatStore.getState().providerEpoch
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'claude'))
        result.current.beginAcc('streamB', 'threadB', seedThread('threadB', 'streamB', 'azure-foundry'))
      })
      act(() => {
        capturedHandler!.onDone(1, 1, 'streamA', {servedModel: 'claude-sonnet-4-20250514'})
        capturedHandler!.onDone(1, 1, 'streamB', {})
      })
      expect(useChatStore.getState().providerEpoch).toBe(epoch0)
      expect(useChatStore.getState().conversations.get('threadB')![0]).toBeDefined()
    })

    it('titles a provider failure, keeps the server detail, tags the code and refetches on auth/not-configured', () => {
      const {result} = renderEngine()
      const epoch0 = useChatStore.getState().providerEpoch
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'azure-foundry'))
        capturedHandler!.onError('no Azure credentials for this flow', 'streamA', 'PROVIDER_NOT_CONFIGURED')
      })
      const msg = useChatStore.getState().conversations.get('threadA')![0]!
      expect(msg.finishReason).toBe('error')
      expect(msg.errorCode).toBe('PROVIDER_NOT_CONFIGURED')
      expect(msg.content).toBe('**This provider is not set up here** — no Azure credentials for this flow')
      expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)

      act(() => {
        result.current.beginAcc('streamB', 'threadB', seedThread('threadB', 'streamB', 'azure-foundry'))
        capturedHandler!.onError('missing Foundry User role', 'streamB', 'PROVIDER_PERMISSION')
      })
      expect(useChatStore.getState().conversations.get('threadB')![0]!.errorCode).toBe('PROVIDER_PERMISSION')
      expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)
    })

    it('keeps the generic bubble (and no code) for other failures', () => {
      const {result} = renderEngine()
      act(() => {
        result.current.beginAcc('streamA', 'threadA', seedThread('threadA', 'streamA', 'azure-foundry'))
        capturedHandler!.onError('timed out', 'streamA', 'STREAM_TIMEOUT')
      })
      const msg = useChatStore.getState().conversations.get('threadA')![0]!
      expect(msg.content).toBe('*Error: timed out*')
      expect(msg.errorCode).toBeUndefined()
    })
  })
})

function seedSlotThread(threadId: string, streamId: string, provider: string) {
  const st = useChatStore.getState()
  useChatStore.setState({
    threads: [
      ...st.threads,
      {
        id: threadId,
        flowId: 'flow-1',
        title: 't',
        createdAt: '2024',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    conversations: new Map([...st.conversations, [threadId, []]]),
    streams: {
      ...st.streams,
      [threadId]: {
        streamId,
        messageId: `m-${streamId}`,
        text: '',
        isThinking: true,
        tokens: 0,
        toolStatus: null,
        toolCalls: [],
        approvals: [],
      },
    },
  })
  return {provider: provider as never, model: 'my-deployment'}
}

describe('useChatStreamEngine tool-result status clearing', () => {
  it('clears its own running label and re-arms thinking before any text', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
    })
    act(() => {
      capturedHandler!.onToolStatus!('Agent - web search', 'streamA')
    })
    let slot = useChatStore.getState().streams.threadA
    expect(slot?.toolStatus).toBe('Agent - web search')
    expect(slot?.isThinking).toBe(false)

    act(() => {
      capturedHandler!.onToolResult!(
        {kind: 'tool-result', name: 'web_search', label: 'Agent - web search', ok: true, durationMs: 5, summary: ''},
        'streamA',
      )
    })
    slot = useChatStore.getState().streams.threadA
    expect(slot?.toolStatus).toBeNull()
    expect(slot?.isThinking).toBe(true)
    expect(slot?.toolCalls).toHaveLength(1)

    act(() => {
      capturedHandler!.onChunk('answer', 'streamA')
    })
    expect(useChatStore.getState().streams.threadA?.isThinking).toBe(false)
  })

  it('keeps isThinking off when answer text already exists', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
      capturedHandler!.onChunk('preamble ', 'streamA')
      capturedHandler!.onToolStatus!('Using search_flow', 'streamA')
    })
    act(() => {
      capturedHandler!.onToolResult!(
        {kind: 'tool-result', name: 'search_flow', label: 'Using search_flow', ok: true, durationMs: 1, summary: ''},
        'streamA',
      )
    })
    const slot = useChatStore.getState().streams.threadA
    expect(slot?.toolStatus).toBeNull()
    expect(slot?.isThinking).toBe(false)
  })

  it('keeps a newer tool label when an older same-phase result lands', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
      capturedHandler!.onToolStatus!('step A', 'streamA')
      capturedHandler!.onToolStatus!('step B', 'streamA')
    })
    act(() => {
      capturedHandler!.onToolResult!(
        {kind: 'tool-result', name: 'a', label: 'step A', ok: true, durationMs: 1, summary: ''},
        'streamA',
      )
    })
    expect(useChatStore.getState().streams.threadA?.toolStatus).toBe('step B')
    act(() => {
      capturedHandler!.onToolResult!(
        {kind: 'tool-result', name: 'b', label: 'step B', ok: true, durationMs: 1, summary: ''},
        'streamA',
      )
    })
    expect(useChatStore.getState().streams.threadA?.toolStatus).toBeNull()
  })
})

describe('useChatStreamEngine error usage', () => {
  it('counts a billed failure: usage and rates land on the bubble and the totals move', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
      capturedHandler!.onChunk('partial ', 'streamA')
    })
    act(() => {
      capturedHandler!.onError('raise Max tokens', 'streamA', 'PROVIDER_OUTPUT_LIMIT', {
        tokensIn: 50,
        tokensOut: 4096,
        meta: {servedModel: 'gpt-5.1'},
      })
    })
    const st = useChatStore.getState()
    const msgs = st.conversations.get('threadA')!
    expect(msgs).toHaveLength(1)
    expect(msgs[0]).toMatchObject({
      finishReason: 'error',
      errorCode: 'PROVIDER_OUTPUT_LIMIT',
      tokensIn: 50,
      tokensOut: 4096,
      servedModel: 'gpt-5.1',
    })
    const thread = st.threads.find(t => t.id === 'threadA')!
    expect(thread.tokensIn).toBe(50)
    expect(thread.tokensOut).toBe(4096)
  })

  it('an unbilled error carries no usage keys and moves no totals', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
      capturedHandler!.onError('timed out', 'streamA')
    })
    const st = useChatStore.getState()
    const msgs = st.conversations.get('threadA')!
    expect('tokensIn' in msgs[0]!).toBe(false)
    expect('tokensOut' in msgs[0]!).toBe(false)
    expect('servedModel' in msgs[0]!).toBe(false)
    expect(st.threads.find(t => t.id === 'threadA')!.tokensIn).toBe(0)
  })

  it('flags a shared-credential failure on the error bubble', () => {
    const {result} = renderEngine()
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
      capturedHandler!.onError('ask your organisation admin', 'streamA', 'PROVIDER_NOT_CONFIGURED', {shared: true})
    })
    expect(useChatStore.getState().conversations.get('threadA')![0]!.errorShared).toBe(true)
  })
})

describe('useChatStreamEngine resumeAfterReload', () => {
  function seedIdleThread(threadId: string) {
    const st = useChatStore.getState()
    useChatStore.setState({
      threads: [
        ...st.threads,
        {
          id: threadId,
          flowId: 'flow-1',
          title: 't',
          createdAt: '2024',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
        },
      ],
      conversations: new Map([...st.conversations, [threadId, []]]),
    })
  }

  it('re-attaches an in-flight stream and commits the resumed done with its served model and rates', async () => {
    const {result} = renderEngine()
    seedIdleThread('threadA')
    const gone = vi.fn()

    let accepted: boolean | undefined
    await act(async () => {
      accepted = result.current.resumeAfterReload('threadA', 'live-1', gone)
    })
    expect(accepted).toBe(true)
    const slot = useChatStore.getState().streams.threadA
    expect(slot?.streamId).toBe('live-1')
    expect(slot?.isThinking).toBe(true)
    expect(capturedResume?.streamId).toBe('live-1')
    expect(capturedResume?.opts?.mode).toBe('full')

    await act(async () => {
      capturedHandler!.onReplace('the answer', 'live-1')
      capturedHandler!.onDone(7, 5, 'live-1', {servedModel: 'gpt-4o'})
    })
    const st = useChatStore.getState()
    const committed = st.conversations.get('threadA')![0]
    expect(committed).toMatchObject({
      content: 'the answer',
      servedModel: 'gpt-4o',
      tokensIn: 5,
      tokensOut: 7,
    })
    expect(st.streams.threadA).toBeUndefined()
    expect(gone).not.toHaveBeenCalled()
  })

  it('drops the slot and reports the loss when the stream is gone (403/404)', async () => {
    const {result} = renderEngine()
    seedIdleThread('threadA')
    const gone = vi.fn()
    await act(async () => {
      result.current.resumeAfterReload('threadA', 'live-2', gone)
    })
    expect(useChatStore.getState().streams.threadA?.streamId).toBe('live-2')

    act(() => {
      capturedResume?.opts?.onGone?.()
    })
    expect(useChatStore.getState().streams.threadA).toBeUndefined()
    expect(gone).toHaveBeenCalledTimes(1)
  })

  it('drops the slot and reports the loss when the event connection never opens', async () => {
    registerStreamImpl = () => Promise.reject(new Error('cannot connect'))
    const {result} = renderEngine()
    seedIdleThread('threadA')
    const gone = vi.fn()
    await act(async () => {
      result.current.resumeAfterReload('threadA', 'live-4', gone)
    })
    expect(useChatStore.getState().streams.threadA).toBeUndefined()
    expect(capturedResume).toBeNull()
    expect(gone).toHaveBeenCalledTimes(1)
  })

  it('refuses to re-attach a thread that already streams', async () => {
    const {result} = renderEngine()
    seedSlotThread('threadA', 'streamA', 'azure-foundry')
    let accepted: boolean | undefined
    await act(async () => {
      accepted = result.current.resumeAfterReload('threadA', 'live-3')
    })
    expect(accepted).toBe(false)
    expect(capturedResume).toBeNull()
    expect(useChatStore.getState().streams.threadA?.streamId).toBe('streamA')
  })
})

describe('useChatStreamEngine served-model refetch key', () => {
  it('refetches for a second deployment and for a router that changes served model', () => {
    const {result} = renderEngine()
    const epoch0 = useChatStore.getState().providerEpoch
    act(() => {
      result.current.beginAcc('streamA', 'threadA', seedSlotThread('threadA', 'streamA', 'azure-foundry'))
    })
    act(() => {
      capturedHandler!.onDone(1, 1, 'streamA', {servedModel: 'gpt-4o'})
    })
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)

    act(() => {
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
            approvals: [],
          },
        },
      })
      result.current.beginAcc('streamB', 'threadA', {provider: 'azure-foundry' as never, model: 'other-deployment'})
    })
    act(() => {
      capturedHandler!.onDone(1, 1, 'streamB', {servedModel: 'gpt-4o'})
    })
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 2)

    act(() => {
      useChatStore.setState({
        streams: {
          threadA: {
            streamId: 'streamC',
            messageId: 'mC',
            text: '',
            isThinking: true,
            tokens: 0,
            toolStatus: null,
            toolCalls: [],
            approvals: [],
          },
        },
      })
      result.current.beginAcc('streamC', 'threadA', {provider: 'azure-foundry' as never, model: 'model-router'})
    })
    act(() => {
      capturedHandler!.onDone(1, 1, 'streamC', {servedModel: 'gpt-4o'})
    })
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 3)
    act(() => {
      useChatStore.setState({
        streams: {
          threadA: {
            streamId: 'streamD',
            messageId: 'mD',
            text: '',
            isThinking: true,
            tokens: 0,
            toolStatus: null,
            toolCalls: [],
            approvals: [],
          },
        },
      })
      result.current.beginAcc('streamD', 'threadA', {provider: 'azure-foundry' as never, model: 'model-router'})
    })
    act(() => {
      capturedHandler!.onDone(1, 1, 'streamD', {servedModel: 'gpt-4o-mini'})
    })
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 4)

    act(() => {
      useChatStore.setState({
        streams: {
          threadA: {
            streamId: 'streamE',
            messageId: 'mE',
            text: '',
            isThinking: true,
            tokens: 0,
            toolStatus: null,
            toolCalls: [],
            approvals: [],
          },
        },
      })
      result.current.beginAcc('streamE', 'threadA', {provider: 'azure-foundry' as never, model: 'model-router'})
    })
    act(() => {
      capturedHandler!.onDone(1, 1, 'streamE', {servedModel: 'gpt-4o-mini'})
    })
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 4)
  })
})

describe('useChatStreamEngine model labels', () => {
  const DOCS = {provider: 'azure-foundry' as never, model: 'm/docs000000', modelLabel: 'Docs writer'}

  it('commits the reply and the error bubble with the label they were sent with', () => {
    const {result} = renderEngine()
    act(() => {
      seedSlotThread('threadA', 'streamA', 'azure-foundry')
      result.current.beginAcc('streamA', 'threadA', DOCS)
      capturedHandler!.onChunk('the docs', 'streamA')
      capturedHandler!.onDone(3, 2, 'streamA')
    })
    expect(useChatStore.getState().conversations.get('threadA')![0]).toMatchObject({
      provider: 'azure-foundry',
      model: 'm/docs000000',
      modelLabel: 'Docs writer',
      content: 'the docs',
    })

    act(() => {
      seedSlotThread('threadB', 'streamB', 'azure-foundry')
      result.current.beginAcc('streamB', 'threadB', DOCS)
      capturedHandler!.onError('boom', 'streamB')
    })
    expect(useChatStore.getState().conversations.get('threadB')![0]).toMatchObject({
      finishReason: 'error',
      modelLabel: 'Docs writer',
    })
  })

  it('a stream reports MODEL_NOT_AVAILABLE: a coded bubble (Choose another model) and a listing refresh', () => {
    const {result} = renderEngine()
    const epoch0 = useChatStore.getState().providerEpoch
    act(() => {
      seedSlotThread('threadA', 'streamA', 'azure-foundry')
      result.current.beginAcc('streamA', 'threadA', DOCS)
      capturedHandler!.onError('the model is not available', 'streamA', 'MODEL_NOT_AVAILABLE')
    })
    const msg = useChatStore.getState().conversations.get('threadA')![0]!
    expect(msg.errorCode).toBe('MODEL_NOT_AVAILABLE')
    expect(msg.content).toBe('**This model is no longer available** — the model is not available')
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)
  })

  function seedIdle(threadId: string, extra: Record<string, unknown> = {}, messages: object[] = []) {
    const st = useChatStore.getState()
    useChatStore.setState({
      threads: [
        ...st.threads,
        {
          id: threadId,
          flowId: 'flow-1',
          title: 't',
          createdAt: '2024',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
          ...extra,
        },
      ],
      conversations: new Map([...st.conversations, [threadId, messages as never]]),
    })
  }

  it('labels a re-attached stream with its turn’s record: the model it went to, not the thread’s pin', async () => {
    const {result} = renderEngine()
    seedIdle('threadA', {
      ...DOCS,
      inFlight: {
        streamId: 'live-1',
        messageId: 'a-live-1',
        provider: 'azure-foundry',
        model: 'm/gpt4100000',
        modelLabel: 'GPT-4.1',
      },
    })
    useChatStore.setState({historyStatus: {threadA: LOADED}})
    await act(async () => {
      result.current.resumeAfterReload('threadA', 'live-1')
    })
    await act(async () => {
      capturedHandler!.onReplace('resumed', 'live-1')
      capturedHandler!.onDone(1, 1, 'live-1')
    })
    expect(useChatStore.getState().conversations.get('threadA')!.at(-1)).toMatchObject({
      id: 'a-live-1',
      provider: 'azure-foundry',
      model: 'm/gpt4100000',
      modelLabel: 'GPT-4.1',
    })
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    const thread = useChatStore.getState().threads.find(t => t.id === 'threadA')!
    expect(thread.inFlight).toBeUndefined()
    expect(thread).toMatchObject(DOCS)
  })

  it('guesses nothing without a record: no model on the answer, and no save over the server’s copy', async () => {
    const s = useSettingsStore.getState().settings
    useSettingsStore.setState({settings: {...s, ai: {...s.ai, activeProvider: 'claude'}}})
    const {result} = renderEngine()
    seedIdle(
      'threadA',
      {inFlight: {streamId: 'older', provider: 'openai', model: 'gpt-5'}},
      [
        {id: 'u1', role: 'user', content: 'q', timestamp: 't'},
        {id: 'a1', role: 'assistant', content: 'a', timestamp: 't', provider: 'openai', model: 'gpt-5'},
        {id: 'u2', role: 'user', content: 'q2', timestamp: 't'},
      ],
    )
    useChatStore.setState({historyStatus: {threadA: LOADED}})
    await act(async () => {
      result.current.resumeAfterReload('threadA', 'live-2')
    })
    await act(async () => {
      capturedHandler!.onReplace('resumed', 'live-2')
      capturedHandler!.onDone(1, 1, 'live-2')
    })
    const answer = useChatStore.getState().conversations.get('threadA')!.at(-1)!
    expect(answer.content).toBe('resumed')
    expect(answer).not.toHaveProperty('provider')
    expect(answer).not.toHaveProperty('model')
    expect(answer).not.toHaveProperty('modelLabel')
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })
})

// The history-truncation notice (R-streaming-7): the stream slot keeps the count, the committed message stores it
// (the chip survives a reload), and a resume replays it from the journal.
describe('useChatStreamEngine history truncation notice', () => {
  function setupThread(result: ReturnType<typeof renderEngine>['result']) {
    act(() => {
      useChatStore.setState({
        threads: [
          {
            id: 'threadA',
            flowId: 'flow-1',
            title: 'T',
            createdAt: '2024',
            contextBlockId: null,
            selectedSourceFiles: [],
            tokensIn: 0,
            tokensOut: 0,
          },
        ],
        conversations: new Map([['threadA', []]]),
      })
      result.current.beginAcc('streamA', 'threadA', META)
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
            approvals: [],
          },
        },
      })
    })
  }

  it('stores the omitted count on the committed message', () => {
    const {result} = renderEngine()
    setupThread(result)
    act(() => {
      capturedHandler!.onNotice!({kind: 'notice', notice: 'history_truncated', dropped: 4, keptFrom: 4}, 'streamA')
      capturedHandler!.onChunk('answer', 'streamA')
    })
    expect(useChatStore.getState().streams.threadA!.truncated).toBe(4)
    act(() => {
      capturedHandler!.onDone(2, 10, 'streamA')
    })
    const msgs = useChatStore.getState().conversations.get('threadA')
    expect(msgs).toHaveLength(1)
    expect(msgs![0]!.truncated).toBe(4)
  })

  it('keeps the latest keptFrom when a later iteration drops more', () => {
    const {result} = renderEngine()
    setupThread(result)
    act(() => {
      capturedHandler!.onNotice!({kind: 'notice', notice: 'history_truncated', dropped: 4, keptFrom: 4}, 'streamA')
      capturedHandler!.onNotice!({kind: 'notice', notice: 'history_truncated', dropped: 2, keptFrom: 6}, 'streamA')
    })
    expect(useChatStore.getState().streams.threadA!.truncated).toBe(6)
  })

  it('rebuilds the count from a resumed journal', () => {
    const {result} = renderEngine()
    setupThread(result)
    act(() => {
      capturedHandler!.onResumeState!(
        [{kind: 'notice', notice: 'history_truncated', dropped: 3, keptFrom: 3}],
        'streamA',
      )
    })
    expect(useChatStore.getState().streams.threadA!.truncated).toBe(3)
  })

  it('leaves the message without a count when nothing was omitted', () => {
    const {result} = renderEngine()
    setupThread(result)
    act(() => {
      capturedHandler!.onChunk('answer', 'streamA')
      capturedHandler!.onDone(2, 10, 'streamA')
    })
    expect(useChatStore.getState().conversations.get('threadA')![0]!.truncated).toBeUndefined()
  })
})
