import {describe, it, expect, beforeEach, afterEach, vi} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatThreads, COMPACT_KEEP_PAIRS, lastExchanges} from './useChatThreads'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import {useSettingsStore} from '@/stores/settingsStore'
import i18n from '@/i18n'
import {registerCurrentUserResolver} from '@/stores/storeRegistry'
import {rekeyThread, resetThreadIndexForTest, startTitleSync, TITLE_SYNC_DEBOUNCE_MS} from '@/lib/threadIndex'
import type {ChatMessage, FlowDocument} from '@/types'

vi.mock('@/api', () => ({
  chatApi: {
    saveConversation: vi.fn().mockResolvedValue(undefined),
    clearConversation: vi.fn().mockResolvedValue(undefined),
    getConversation: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    setThreadTitle: vi.fn().mockResolvedValue({}),
  },
}))

const toastMock = vi.hoisted(() => ({warning: vi.fn(), error: vi.fn(), success: vi.fn(), info: vi.fn()}))
vi.mock('@/components/shared/Toast', () => ({useOptionalToast: () => toastMock}))

const doc = {id: 'flow1', name: 'Flow'} as FlowDocument
const initialSettingsState = useSettingsStore.getState()
const initialChatState = useChatStore.getState()

function msg(id: string, role: ChatMessage['role']): ChatMessage {
  return {id, role, content: id, timestamp: '2024-01-01T00:00:00Z'}
}

function seedThread(pairs: number, contextBlockId: string | null = null): string {
  const id = useChatStore.getState().createThread('flow1')
  useChatStore.getState().updateThread(id, {contextBlockId})
  for (let i = 0; i < pairs; i++) {
    useChatStore.getState().appendMessage(id, msg(`u${i}`, 'user'))
    useChatStore.getState().appendMessage(id, msg(`a${i}`, 'assistant'))
  }
  return id
}

function renderThreads(activeThreadId: string) {
  return renderHook(() => useChatThreads({doc, activeThreadId, sourceFiles: []}))
}

beforeEach(() => {
  useChatStore.setState(initialChatState, true)
  vi.mocked(chatApi.clearConversation).mockClear()
  vi.mocked(chatApi.saveConversation).mockClear()
  toastMock.warning.mockClear()
})

describe('useChatThreads server-side history', () => {
  it('clear deletes the backend conversation, not just the local one', async () => {
    const id = seedThread(2)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id, 0)
  })

  it('clear targets the thread key even for a block-scoped thread', async () => {
    const id = seedThread(2, 'block-7')
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id, 0)
    expect(useChatStore.getState().threads.find(t => t.id === id)?.contextBlockId).toBeNull()
  })

  it('clear drops the whole block scope: the anchor and the lost-block notice go with the id', async () => {
    const id = seedThread(1)
    useChatStore.getState().updateThread(id, {
      contextBlockId: null,
      contextBlockAnchor: {subflow: 'Main', rawType: 'HTTPClient.InvokeUrl', name: 'Fetch', ordinal: 0, count: 1},
      contextBlockLost: 'Fetch',
    })
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.contextBlockAnchor).toBeNull()
    expect(thread.contextBlockLost).toBeUndefined()
  })

  it('compact persists the trimmed history so the next prompt is actually cheaper', async () => {
    const id = seedThread(6)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const kept = useChatStore.getState().getMessages(id)
    expect(kept).toHaveLength(COMPACT_KEEP_PAIRS * 2)
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    const [flowId, scope, saved] = vi.mocked(chatApi.saveConversation).mock.calls[0]!
    expect(flowId).toBe('flow1')
    expect(scope).toBe(id)
    expect(saved.map(m => m.id)).toEqual(kept.map(m => m.id))
  })

  it('compact is a no-op below the keep threshold and writes nothing', async () => {
    const id = seedThread(1)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(2)
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
  })
})

describe('useChatThreads failure paths', () => {
  it('a failed clear keeps the thread as it was', async () => {
    const id = seedThread(2)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(new Error('403'))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(4)
  })

  it('a failed compact keeps every message locally', async () => {
    const id = seedThread(6)
    vi.mocked(chatApi.saveConversation).mockRejectedValueOnce(new Error('403'))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(12)
  })

  it('refuses to compact a thread whose server history has not loaded', async () => {
    const id = seedThread(6)
    useChatStore.setState(s => ({historyStatus: {...s.historyStatus, [id]: {state: 'error', token: 99}}}))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(chatApi.saveConversation).not.toHaveBeenCalled()
    expect(useChatStore.getState().getMessages(id)).toHaveLength(12)
  })

  it('never saves transient error bubbles as part of the compacted history', async () => {
    const id = seedThread(3)
    useChatStore
      .getState()
      .appendMessage(id, {...msg('boom', 'assistant'), finishReason: 'error', content: '*Error: provider exploded*'})
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const saved = vi.mocked(chatApi.saveConversation).mock.calls[0]![2]
    expect(saved.some(m => m.finishReason === 'error')).toBe(false)
  })
})

describe('compact keeps whole exchanges', () => {
  it('applies the exact list it persisted, error bubbles dropped', async () => {
    const id = seedThread(4)
    useChatStore.getState().appendMessage(id, {...msg('boom', 'assistant'), finishReason: 'error'})
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const saved = vi.mocked(chatApi.saveConversation).mock.calls[0]![2]
    expect(saved.map(m => m.id)).toEqual(['u1', 'a1', 'u2', 'a2', 'u3', 'a3'])
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(saved.map(m => m.id))
  })

  it('lastExchanges keeps the last N user turns with their replies', () => {
    const list = [
      msg('u0', 'user'),
      msg('a0', 'assistant'),
      msg('u1', 'user'),
      msg('a1', 'assistant'),
      msg('u2', 'user'),
    ]
    expect(lastExchanges(list, 2).map(m => m.id)).toEqual(['u1', 'a1', 'u2'])
    expect(lastExchanges(list, 9)).toHaveLength(5)
  })
})

describe('useChatThreads when the conversation changed elsewhere (ADR 0031)', () => {
  const conflict = () => Object.assign(new Error('changed'), {status: 409, code: 'CONVERSATION_CONFLICT'})

  it('a clear refused as stale keeps the conversation and shows what the server holds', async () => {
    const id = seedThread(2)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(conflict())
    vi.mocked(chatApi.getConversation).mockResolvedValueOnce({
      messages: [msg('u0', 'user'), msg('a0', 'assistant'), msg('s1', 'user')],
      version: 7,
    })
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(['u0', 'a0', 's1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(7)
    expect(chatApi.clearConversation).toHaveBeenCalledTimes(1)
  })

  it('a compaction refused as stale is not retried and leaves the server list in place', async () => {
    const id = seedThread(6)
    vi.mocked(chatApi.saveConversation).mockRejectedValueOnce(conflict())
    vi.mocked(chatApi.getConversation).mockResolvedValueOnce({
      messages: [msg('u0', 'user'), msg('s1', 'user')],
      version: 8,
    })
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(['u0', 's1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(8)
  })
})

describe('closing a thread deletes its stored conversation (R-frontend-14, R-frontend-c26)', () => {
  const conflict = () => Object.assign(new Error('changed'), {status: 409, code: 'CONVERSATION_CONFLICT'})
  const notFound = () => Object.assign(new Error('not found'), {status: 404, code: 'NOT_FOUND'})

  afterEach(() => {
    useSettingsStore.setState(initialSettingsState, true)
  })

  it('clears the loaded thread on the server with its key and base version before it goes', async () => {
    const id = seedThread(2)
    useChatStore.getState().setConversationVersion(id, 3)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id, 3)
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })

  it('a 409 keeps the thread open, shows the server list and says it changed elsewhere', async () => {
    const id = seedThread(1)
    useChatStore.getState().setConversationVersion(id, 3)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(conflict())
    vi.mocked(chatApi.getConversation).mockResolvedValueOnce({
      messages: [msg('u0', 'user'), msg('x', 'user')],
      version: 5,
    })
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(true)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(5)
    expect(toastMock.warning).toHaveBeenCalledWith(
      'This conversation changed in another tab or session, so it was reloaded. Nothing was changed; try again.',
    )
  })

  it('a 404 (nothing stored) still closes the thread', async () => {
    const id = seedThread(1)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(notFound())
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })

  it('a thread that never stored anything closes without a server call', async () => {
    const id = useChatStore.getState().createThread('flow1')
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).not.toHaveBeenCalled()
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })

  it('with history saving off there is nothing stored to clear', async () => {
    useSettingsStore.setState(s => ({
      settings: {...s.settings, ai: {...s.settings.ai, saveConversationHistory: false}},
    }))
    const id = seedThread(2)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).not.toHaveBeenCalled()
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })

  it('a thread closed mid-stream is cleared whole: its own stopped turn moves the version', async () => {
    const id = seedThread(1)
    useChatStore.getState().setConversationVersion(id, 3)
    useChatStore.getState().startStream(id, 'sid-1', 'msg-1')
    vi.mocked(chatApi.clearConversation).mockClear()
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id)
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })

  it('the close confirmation says the messages are deleted', () => {
    expect(i18n.t('chat:threads.confirmClose', {name: 'Retry loop'})).toMatch(/messages will be deleted/)
  })

  it('a thread whose history has not loaded is cleared whole (no base version)', async () => {
    const id = seedThread(0)
    useChatStore.getState().beginHistoryLoad(id)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id)
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
  })
})

// R-security-3: a conversation stored before owners can be cleared only by someone who may delete the flow; anyone
// else drops the thread from their own list, and is told. A thread moved to a new key says so.
describe('conversations with owners', () => {
  const unowned = () => Object.assign(new Error('unowned'), {status: 403, code: 'CONVERSATION_UNOWNED'})
  const UNOWNED_CLOSED =
    'This conversation was stored before conversations had owners, so only someone who can delete the flow can delete it: it stays on the server. It was removed from your list.'
  const UNOWNED_CLEARED =
    'This conversation was stored before conversations had owners, so only someone who can delete the flow can delete it: it stays on the server. Your thread starts afresh.'

  beforeEach(() => {
    toastMock.info.mockClear()
    toastMock.error.mockClear()
    resetThreadIndexForTest()
    registerCurrentUserResolver(() => 'user-1')
  })
  afterEach(() => {
    registerCurrentUserResolver(() => null)
  })

  it('a close refused as unowned still drops the thread, and says why', async () => {
    const id = seedThread(1)
    useChatStore.getState().setConversationVersion(id, 2)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(unowned())
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
    expect(toastMock.info).toHaveBeenCalledWith(UNOWNED_CLOSED)
  })

  // The user asked for an empty thread, not for the thread to go: it starts afresh under a new key, as Clear would
  // leave it, and the old conversation stays on the server, where the user can no longer reach it by this thread.
  it('a clear refused as unowned starts the thread afresh under a new key, and says why', async () => {
    const id = seedThread(1)
    useChatStore.getState().setConversationVersion(id, 2)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(unowned())
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    const st = useChatStore.getState()
    expect(st.threads.some(t => t.id === id)).toBe(false)
    const fresh = st.getFlowThreads('flow1')
    expect(fresh).toHaveLength(1)
    expect(st.getMessages(fresh[0]!.id)).toHaveLength(0)
    expect(st.activeThreadId).toBe(fresh[0]!.id)
    expect(toastMock.info).toHaveBeenCalledWith(UNOWNED_CLEARED)
    expect(toastMock.info).toHaveBeenCalledTimes(1)
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('a compaction stored under a new key is not reported as changed elsewhere', async () => {
    const id = seedThread(6)
    vi.mocked(chatApi.saveConversation)
      .mockRejectedValueOnce(Object.assign(new Error('taken'), {status: 409, code: 'CONVERSATION_KEY_TAKEN'}))
      .mockResolvedValueOnce({version: 1})
    toastMock.warning.mockClear()
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(toastMock.warning).not.toHaveBeenCalled()
    const moved = useChatStore.getState().getFlowThreads('flow1')[0]!
    expect(moved.id).not.toBe(id)
    expect(useChatStore.getState().getMessages(moved.id)).toHaveLength(COMPACT_KEEP_PAIRS * 2)
  })

  it('closing a renamed empty thread deletes the row its name made, so the index does not bring it back', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow1')
    vi.mocked(chatApi.setThreadTitle).mockResolvedValueOnce({version: 1})
    act(() => {
      useChatStore.getState().updateThread(id, {title: 'Named'})
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    })
    vi.useRealTimers()
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCloseThread(id))

    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id, 1)
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(false)
    stop()
  })

  it('says so when a thread moves to a new key', async () => {
    const id = seedThread(1)
    renderThreads(id)
    act(() => {
      rekeyThread(id)
    })
    expect(toastMock.info).toHaveBeenCalledWith(
      "This thread's id belonged to another user's conversation, so it continues under a new id. Your messages are kept.",
    )
  })
})
