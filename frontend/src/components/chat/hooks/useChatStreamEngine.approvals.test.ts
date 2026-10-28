import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {usePresenceStore} from '@/stores/presenceStore'
import {parseChatEvent, type ChatEvent} from '@/lib/chatEvent'
import {chatEventGolden} from '@/testing/chatEventGoldens'
import type {StreamHandler} from '@/hooks/useStreamingMessage'
import type {FlowDocument} from '@/types'

// Approval cards through the stream engine (R-approvals-4, R-approvals-5, R-frontend-8), fed with the backend's own
// event goldens (R-testing-3) rather than hand-written payloads.

let capturedHandler: StreamHandler | null = null
const teardown = vi.fn()

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: (handler: StreamHandler) => {
    capturedHandler = handler
    return {
      registerStream: async () => true,
      confirmStream: vi.fn(),
      cancel: vi.fn(),
      teardownStream: teardown,
      resumeInto: vi.fn(),
    }
  },
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    streamMessage: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
    respondApprovalDecision: vi.fn().mockResolvedValue(undefined),
  },
  flowApi: {loadFlowFromPath: vi.fn().mockResolvedValue(null)},
  analysisApi: {analyzeFlowById: vi.fn().mockResolvedValue({flowId: 'flow-1', findings: []})},
}))

vi.mock('@/api/library', () => ({
  libraryApi: {
    get: vi.fn().mockResolvedValue({id: 'flow-1', version: 7}),
    getContent: vi.fn().mockResolvedValue({id: 'flow-1', name: 'fixed', subflows: []}),
  },
}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'})}),
}))

import {useChatStreamEngine} from './useChatStreamEngine'
import {rekeyThread} from '@/lib/threadIndex'
import {chatApi, flowApi} from '@/api'
import {libraryApi} from '@/api/library'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

/** The golden event re-addressed to streamA, parsed as the SSE path parses it. */
function golden<K extends ChatEvent['kind']>(name: string): Extract<ChatEvent, {kind: K}> {
  const ev = parseChatEvent(chatEventGolden(name, 'streamA'))
  if (!ev) throw new Error(`golden ${name} does not parse`)
  return ev as Extract<ChatEvent, {kind: K}>
}

function proposal() {
  capturedHandler!.onFixProposal!(golden<'fix-proposal'>('fix_proposal_single'), 'streamA')
}

function decision(name: string) {
  const d = golden<'fix-decision'>(name)
  capturedHandler!.onFixDecision!(d.proposalId, d.status, d.message, 'streamA', d.items)
}

function setup(serverOwned = false) {
  const hook = renderHook(() => useChatStreamEngine())
  act(() => {
    hook.result.current.beginAcc('streamA', 'threadA', {...META, serverOwned})
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
      historyStatus: {threadA: {state: 'loaded' as const, token: 1}},
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

const committed = () => useChatStore.getState().conversations.get('threadA') ?? []
const flush = () => new Promise(r => setTimeout(r, 0))

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    cb()
    return 0
  })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  useChatStore.setState({threads: [], streams: {}, activeThreadId: null, conversations: new Map()})
  useFlowStore.setState({readOnly: false})
})

describe('a turn that fails after a fix (R-approvals-4)', () => {
  it('keeps the applied card on the error message and saves it', async () => {
    setup()
    act(() => {
      proposal()
      decision('fix_decision_applied')
      capturedHandler!.onError('the provider is unavailable', 'streamA', 'PROVIDER_UNAVAILABLE')
    })
    const msgs = committed()
    expect(msgs).toHaveLength(1)
    expect(msgs[0]!.finishReason).toBe('error')
    expect(msgs[0]!.fixProposals).toHaveLength(1)
    expect(msgs[0]!.fixProposals![0]!.status).toBe('applied')
    await flush()
    expect(chatApi.saveConversation).toHaveBeenCalledWith(
      'flow-1',
      'threadA',
      expect.arrayContaining([expect.objectContaining({id: msgs[0]!.id, fixProposals: msgs[0]!.fixProposals})]),
    )
  })

  it('leaves the save to the server when it owns the turn', async () => {
    setup(true)
    act(() => {
      proposal()
      decision('fix_decision_applied')
      capturedHandler!.onError('the provider is unavailable', 'streamA', 'PROVIDER_UNAVAILABLE')
    })
    expect(committed()[0]!.fixProposals![0]!.status).toBe('applied')
    await flush()
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })
})

describe('a stop while a fix is being applied (R-approvals-5)', () => {
  it('commits the card, then the late decision settles and saves it', async () => {
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]!.status).toBe('applying')
    act(() => result.current.stop('threadA'))
    expect(committed()).toHaveLength(1)
    expect(committed()[0]!.fixProposals![0]!.status).toBe('applying')
    expect(teardown).not.toHaveBeenCalled()

    act(() => decision('fix_decision_applied'))
    expect(committed()[0]!.fixProposals![0]!.status).toBe('applied')
    expect(committed()[0]!.fixProposals![0]!.items![0]!.status).toBe('applied')
    await flush()
    expect(chatApi.saveConversation).toHaveBeenLastCalledWith(
      'flow-1',
      'threadA',
      expect.arrayContaining([expect.objectContaining({fixProposals: [expect.objectContaining({status: 'applied'})]})]),
    )

    act(() => capturedHandler!.onError('response stopped', 'streamA', 'STREAM_STOPPED'))
    expect(teardown).toHaveBeenCalledWith('streamA')
    expect(committed()).toHaveLength(1)
  })

  it('commits a card even when no text streamed', () => {
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    act(() => result.current.stop('threadA'))
    expect(committed()).toHaveLength(1)
    expect(committed()[0]!.content).toBe('')
    expect(committed()[0]!.fixProposals).toHaveLength(1)
  })

  // Closing deletes the thread and its stored conversation (R-frontend-c26): the turn is dropped, card and all,
  // and the late decision finds nothing to settle.
  it('closing the thread mid-apply drops the turn, card and all', () => {
    const {result} = setup()
    act(() => {
      capturedHandler!.onChunk('Applying the fix now', 'streamA')
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    act(() => result.current.abandon('threadA'))
    expect(committed()).toHaveLength(0)
    act(() => decision('fix_decision_applied'))
    expect(committed()).toHaveLength(0)
  })
})

describe('closing the thread and a restarting server (step 4 review)', () => {
  it('closing the thread mid-apply saves nothing from the client: the clear that follows would race it', async () => {
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    act(() => {
      result.current.abandon('threadA')
      useChatStore.getState().closeThread('threadA')
    })
    await flush()
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('a decision a rate limit refused goes back to pending, to be sent again (R-streaming-c20)', async () => {
    vi.mocked(chatApi.respondFixDecision).mockRejectedValueOnce(
      Object.assign(new Error('rate limit exceeded'), {status: 429, code: 'RATE_LIMITED'}),
    )
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    await act(async () => {
      await flush()
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]!.status).toBe('pending')
  })

  it('leaves a card the server settled while the refused decision was being retried', async () => {
    let refuse: (err: unknown) => void = () => {}
    vi.mocked(chatApi.respondFixDecision).mockImplementationOnce(
      () => new Promise<void>((_resolve, reject) => (refuse = reject)),
    )
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    act(() => {
      decision('fix_decision_timeout')
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]!.status).toBe('timeout')

    await act(async () => {
      refuse(Object.assign(new Error('rate limit exceeded'), {status: 429, code: 'RATE_LIMITED'}))
      await flush()
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]!.status).toBe('timeout')
  })

  it('settles a card still applying on the thread after it moved to a new key', async () => {
    const {result} = setup()
    act(() => {
      capturedHandler!.onChunk('Applying the fix now', 'streamA')
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    act(() => result.current.stop('threadA'))
    const next = rekeyThread('threadA')!
    expect(next).not.toBeNull()

    act(() => decision('fix_decision_applied'))

    const msgs = useChatStore.getState().conversations.get(next) ?? []
    expect(msgs[0]!.fixProposals![0]!.status).toBe('applied')
  })

  it('an approve the restarting server refused goes back to pending, to be sent again', async () => {
    vi.mocked(chatApi.respondFixDecision).mockRejectedValueOnce(
      Object.assign(new Error('the server is restarting'), {status: 503, code: 'SERVER_RESTARTING'}),
    )
    const {result} = setup()
    act(() => {
      proposal()
      result.current.respondFixProposal('threadA', true, 'proposal-1')
    })
    await act(async () => {
      await flush()
    })
    expect(useChatStore.getState().streams.threadA!.approvals[0]!.status).toBe('pending')
  })
})

describe('the document after a chat-applied fix (R-frontend-8)', () => {
  it('reloads a library flow without the presence socket', async () => {
    usePresenceStore.setState({status: 'disconnected'})
    useFlowStore.setState({document: {id: 'flow-1', name: 'before', subflows: []} as unknown as FlowDocument})
    setup()
    act(() => {
      proposal()
      decision('fix_decision_applied')
    })
    await flush()
    await flush()
    expect(libraryApi.get).toHaveBeenCalledWith('flow-1', undefined)
    expect(libraryApi.getContent).toHaveBeenCalledWith('flow-1', undefined)
    expect(useFlowStore.getState().document?.name).toBe('fixed')
    expect(useFlowStore.getState().libraryVersion).toBe(7)
    expect(flowApi.loadFlowFromPath).not.toHaveBeenCalled()
  })

  it('reloads a desktop flow from its file, as before', async () => {
    useFlowStore.setState({
      document: {id: 'flow-1', name: 'before', filePath: 'C:/flows/Main.txt', subflows: []} as unknown as FlowDocument,
    })
    setup()
    act(() => {
      proposal()
      decision('fix_decision_applied')
    })
    await flush()
    expect(flowApi.loadFlowFromPath).toHaveBeenCalledWith('C:/flows/Main.txt')
    expect(libraryApi.get).not.toHaveBeenCalled()
  })
})
