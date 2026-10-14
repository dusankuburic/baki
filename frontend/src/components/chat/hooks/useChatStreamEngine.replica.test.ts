import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'

import type {StreamHandler} from '@/hooks/useStreamingMessage'

// Stop and approval decision when the server that got the request is not the one running the stream (503
// REPLICA_UNREACHABLE): the card stays answerable and the user is told, with a retry.

let capturedHandler: StreamHandler | null = null

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: (handler: StreamHandler) => {
    capturedHandler = handler
    return {
      registerStream: async () => true,
      confirmStream: vi.fn(),
      cancel: vi.fn(),
      teardownStream: vi.fn(),
      resumeInto: vi.fn(),
    }
  },
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    cancelStream: vi.fn(),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
    respondApprovalDecision: vi.fn().mockResolvedValue(undefined),
  },
  flowApi: {loadFlowFromPath: vi.fn().mockResolvedValue(null)},
  analysisApi: {analyzeFlowById: vi.fn().mockResolvedValue({flowId: 'flow-1', findings: []})},
}))

const toast = {warning: vi.fn(), error: vi.fn(), success: vi.fn(), info: vi.fn(), toast: vi.fn()}
vi.mock('@/components/shared/Toast', () => ({useOptionalToast: () => toast}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'})}),
}))

import {useChatStreamEngine} from './useChatStreamEngine'
import {chatApi} from '@/api'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

function unreachable() {
  return Object.assign(new Error('internal server error'), {
    status: 503,
    code: 'REPLICA_UNREACHABLE',
    details: {retryAfterSeconds: 2},
  })
}

function setup() {
  const hook = renderHook(() => useChatStreamEngine())
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
  return hook
}

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    cb()
    return 0
  })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  useChatStore.setState({threads: [], streams: {}, activeThreadId: null, conversations: new Map()})
})

describe('a decision the stream-running server could not be reached for', () => {
  it('puts the card back to pending and offers a retry that sends the same decision', async () => {
    vi.mocked(chatApi.respondFixDecision).mockRejectedValueOnce(unreachable())
    const {result} = setup()
    await act(async () => {
      result.current.respondFixProposal('threadA', true, undefined, [1])
    })

    expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('pending')
    expect(toast.warning).toHaveBeenCalledTimes(1)
    const opts = toast.warning.mock.calls[0]![1] as {action: {onClick: () => void}}

    await act(async () => {
      opts.action.onClick()
    })
    expect(chatApi.respondFixDecision).toHaveBeenCalledTimes(2)
    expect(chatApi.respondFixDecision).toHaveBeenLastCalledWith('streamA', 'p1', true, [1])
    expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('applying')
  })

  it('keeps marking any other failure as an error, without the retry toast', async () => {
    vi.mocked(chatApi.respondFixDecision).mockRejectedValueOnce(new Error('network'))
    const {result} = setup()
    await act(async () => {
      result.current.respondFixProposal('threadA', true)
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]?.status).toBe('error')
    expect(toast.warning).not.toHaveBeenCalled()
  })
})

describe('a stop the stream-running server could not be reached for', () => {
  it('tells the user the response is still running and offers the stop again', async () => {
    setup()
    act(() => {
      capturedHandler!.onStopUnreachable!('streamA')
    })
    expect(toast.warning).toHaveBeenCalledTimes(1)
    const opts = toast.warning.mock.calls[0]![1] as {action: {onClick: () => void}}

    vi.mocked(chatApi.cancelStream).mockRejectedValueOnce(unreachable())
    await act(async () => {
      opts.action.onClick()
    })
    expect(chatApi.cancelStream).toHaveBeenCalledWith('streamA')
    // Still out of reach: offered again.
    expect(toast.warning).toHaveBeenCalledTimes(2)

    vi.mocked(chatApi.cancelStream).mockResolvedValueOnce(undefined)
    const again = toast.warning.mock.calls[1]![1] as {action: {onClick: () => void}}
    await act(async () => {
      again.action.onClick()
    })
    expect(toast.warning).toHaveBeenCalledTimes(2)
  })
})
