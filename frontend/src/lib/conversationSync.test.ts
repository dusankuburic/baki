import {describe, it, expect, beforeEach, vi} from 'vitest'
import {chatApi} from '@/api'
import {useChatStore} from '@/stores/chatStore'
import type {ChatMessage} from '@/types'
import {
  adoptStreamVersion,
  clearThreadConversation,
  isConversationConflict,
  isPersistable,
  replaceThreadConversation,
  saveThreadConversation,
} from './conversationSync'

vi.mock('@/api', () => ({
  chatApi: {
    saveConversation: vi.fn(),
    getConversation: vi.fn(),
    clearConversation: vi.fn(),
  },
}))

const initialChatState = useChatStore.getState()
const save = vi.mocked(chatApi.saveConversation)
const load = vi.mocked(chatApi.getConversation)
const clear = vi.mocked(chatApi.clearConversation)

function msg(id: string, role: ChatMessage['role'] = 'user', extra: Partial<ChatMessage> = {}): ChatMessage {
  return {id, role, content: id, timestamp: '2024-01-01T00:00:00Z', ...extra}
}

function conflict(version = 9): Error {
  return Object.assign(new Error('changed'), {status: 409, code: 'CONVERSATION_CONFLICT', details: {version}})
}

function seed(messages: ChatMessage[], version?: number): string {
  const st = useChatStore.getState()
  const id = st.createThread('flow1')
  for (const m of messages) st.appendMessage(id, m)
  if (version !== undefined) st.setConversationVersion(id, version)
  return id
}

const ids = (threadId: string) =>
  useChatStore
    .getState()
    .getMessages(threadId)
    .map(m => m.id)

beforeEach(() => {
  useChatStore.setState(initialChatState, true)
  save.mockReset()
  load.mockReset()
  clear.mockReset()
})

describe('isConversationConflict', () => {
  it('is the 409 coded CONVERSATION_CONFLICT, whatever class carries it', () => {
    expect(isConversationConflict(conflict())).toBe(true)
    expect(isConversationConflict(Object.assign(new Error('x'), {status: 409, code: 'CONFLICT'}))).toBe(false)
    expect(isConversationConflict(new Error('boom'))).toBe(false)
    expect(isConversationConflict(null)).toBe(false)
  })
})

describe('saveThreadConversation', () => {
  it('saves on the version the thread holds and remembers the one the server answers', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')])
    save.mockResolvedValue({version: 5})

    await saveThreadConversation(id, 'flow1', id)

    // A new thread starts at 0, "nothing stored yet".
    expect(save).toHaveBeenCalledWith('flow1', id, expect.any(Array), 0)
    expect(save.mock.calls[0]![2].map(m => m.id)).toEqual(['u1', 'a1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(5)
    expect(load).not.toHaveBeenCalled()
  })

  it('saves with no base version for a thread whose version is unknown (a server without versions)', async () => {
    const id = seed([msg('u1')])
    useChatStore.setState(s => ({historyStatus: {...s.historyStatus, [id]: {state: 'loaded', token: 1}}}))
    save.mockResolvedValue({})

    await saveThreadConversation(id, 'flow1', id)

    expect(save.mock.calls[0]).toHaveLength(3)
    expect(useChatStore.getState().getConversationVersion(id)).toBeUndefined()
  })

  it('does not send a failed turn to the server', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant', {finishReason: 'error'})], 2)
    save.mockResolvedValue({version: 3})
    await saveThreadConversation(id, 'flow1', id)
    expect(save.mock.calls[0]![2].map(m => m.id)).toEqual(['u1'])
  })

  it('on a conflict reloads, merges by message id and saves once more on the new version', async () => {
    // The server wrote this turn's question and answer, and another tab added s1; this tab has a local-only l1.
    const id = seed([msg('u1'), msg('l1', 'assistant'), msg('a1', 'assistant')], 2)
    save.mockRejectedValueOnce(conflict(9)).mockResolvedValueOnce({version: 11})
    load.mockResolvedValue({messages: [msg('u1'), msg('s1'), msg('a1', 'assistant')], version: 10})

    await saveThreadConversation(id, 'flow1', id)

    expect(load).toHaveBeenCalledTimes(1)
    expect(save).toHaveBeenCalledTimes(2)
    const [, , resent, base] = save.mock.calls[1]!
    expect(base).toBe(10)
    // Server order kept, the message only this client has kept after the one it followed.
    expect(resent.map(m => m.id)).toEqual(['u1', 'l1', 's1', 'a1'])
    expect(ids(id)).toEqual(['u1', 'l1', 's1', 'a1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(11)
  })

  it('on a conflict where the server already holds every message it adopts the list and saves nothing more', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 2)
    save.mockRejectedValueOnce(conflict(9))
    load.mockResolvedValue({messages: [msg('u1'), msg('a1', 'assistant')], version: 9})

    await saveThreadConversation(id, 'flow1', id)

    expect(save).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(9)
    expect(ids(id)).toEqual(['u1', 'a1'])
  })

  it('a conflict that repeats is settled quietly: bounded rounds, then the server version is adopted, no throw', async () => {
    // Another writer wins every race (a busy thread, a slow answer write): the save never lands, but nothing is wrong.
    const id = seed([msg('u1'), msg('l1')], 2)
    save.mockRejectedValue(conflict(9))
    load
      .mockResolvedValueOnce({messages: [msg('u1')], version: 9})
      .mockResolvedValueOnce({messages: [msg('u1')], version: 10})
      .mockResolvedValueOnce({messages: [msg('u1'), msg('s2')], version: 11})

    await expect(saveThreadConversation(id, 'flow1', id)).resolves.toBeUndefined()

    expect(save).toHaveBeenCalledTimes(3)
    expect(load).toHaveBeenCalledTimes(3)
    // The version the server last reported is the one held, so the next clear or compaction is not made on a stale one.
    expect(useChatStore.getState().getConversationVersion(id)).toBe(11)
    // The message the server never took stays in the thread for the next save.
    expect(ids(id)).toEqual(['u1', 'l1', 's2'])
  })

  it('the clear after a settled conflict uses the adopted version', async () => {
    const id = seed([msg('u1'), msg('l1')], 2)
    save.mockRejectedValue(conflict(9))
    load.mockResolvedValue({messages: [msg('u1')], version: 14})
    await saveThreadConversation(id, 'flow1', id)

    clear.mockResolvedValue(undefined)
    await expect(clearThreadConversation(id, 'flow1', id)).resolves.toBe(true)
    expect(clear).toHaveBeenCalledWith('flow1', id, 14)
  })

  it('a second conflict that the reload shows is already stored needs no third save', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 2)
    save.mockRejectedValue(conflict(9))
    load
      .mockResolvedValueOnce({messages: [msg('u1')], version: 9})
      .mockResolvedValueOnce({messages: [msg('u1'), msg('a1', 'assistant')], version: 10})

    await expect(saveThreadConversation(id, 'flow1', id)).resolves.toBeUndefined()

    expect(save).toHaveBeenCalledTimes(2)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(10)
  })

  it('rethrows any other failure without reloading', async () => {
    const id = seed([msg('u1')], 2)
    save.mockRejectedValue(Object.assign(new Error('down'), {status: 503, code: 'SERVICE_UNAVAILABLE'}))
    await expect(saveThreadConversation(id, 'flow1', id)).rejects.toThrow('down')
    expect(load).not.toHaveBeenCalled()
  })

  it('keeps a local error bubble out of the retry and in the thread', async () => {
    const id = seed([msg('u1'), msg('e1', 'assistant', {finishReason: 'error'}), msg('l1')], 2)
    save.mockRejectedValueOnce(conflict(9)).mockResolvedValueOnce({version: 12})
    load.mockResolvedValue({messages: [msg('u1'), msg('s1')], version: 11})

    await saveThreadConversation(id, 'flow1', id)

    expect(save.mock.calls[1]![2].map(m => m.id)).toEqual(['u1', 'l1', 's1'])
    expect(ids(id)).toEqual(['u1', 'e1', 'l1', 's1'])
  })
})

describe('replaceThreadConversation (compaction)', () => {
  it('stores the kept messages on the held version and remembers the new one', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 4)
    save.mockResolvedValue({version: 6})
    await expect(replaceThreadConversation(id, 'flow1', id, [msg('a1', 'assistant')])).resolves.toBe(true)
    expect(save).toHaveBeenCalledWith('flow1', id, [expect.objectContaining({id: 'a1'})], 4)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(6)
  })

  it('does not retry a conflict: it takes the server list and reports false', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 4)
    save.mockRejectedValue(conflict(8))
    load.mockResolvedValue({messages: [msg('u1'), msg('a1', 'assistant'), msg('s1')], version: 8})

    await expect(replaceThreadConversation(id, 'flow1', id, [msg('a1', 'assistant')])).resolves.toBe(false)

    expect(save).toHaveBeenCalledTimes(1)
    expect(ids(id)).toEqual(['u1', 'a1', 's1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(8)
  })
})

describe('clearThreadConversation', () => {
  it('clears on the held version', async () => {
    const id = seed([msg('u1')], 4)
    clear.mockResolvedValue(undefined)
    await expect(clearThreadConversation(id, 'flow1', id)).resolves.toBe(true)
    expect(clear).toHaveBeenCalledWith('flow1', id, 4)
  })

  it('clears with no base version when the thread has none', async () => {
    const id = seed([msg('u1')])
    useChatStore.setState(s => ({historyStatus: {...s.historyStatus, [id]: {state: 'loaded', token: 1}}}))
    clear.mockResolvedValue(undefined)
    await clearThreadConversation(id, 'flow1', id)
    expect(clear.mock.calls[0]).toHaveLength(2)
  })

  it('on a conflict keeps the conversation: it adopts the server list and reports false', async () => {
    const id = seed([msg('u1')], 4)
    clear.mockRejectedValue(conflict(8))
    load.mockResolvedValue({messages: [msg('u1'), msg('s1')], version: 8})

    await expect(clearThreadConversation(id, 'flow1', id)).resolves.toBe(false)

    expect(clear).toHaveBeenCalledTimes(1)
    expect(ids(id)).toEqual(['u1', 's1'])
    expect(useChatStore.getState().getConversationVersion(id)).toBe(8)
  })

  it('rethrows any other failure', async () => {
    const id = seed([msg('u1')], 4)
    clear.mockRejectedValue(new Error('down'))
    await expect(clearThreadConversation(id, 'flow1', id)).rejects.toThrow('down')
    expect(load).not.toHaveBeenCalled()
  })
})

describe("adoptStreamVersion (the version a stream's terminal event names)", () => {
  it('moves the thread to the stored version when it still held the one the turn began from', () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 6)
    expect(adoptStreamVersion(id, {conversationBase: 6, conversationVersion: 8})).toBe(true)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(8)
  })

  it('leaves the version alone when the thread had moved on: the next write conflicts and reconciles', () => {
    const id = seed([msg('u1')], 5)
    expect(adoptStreamVersion(id, {conversationBase: 6, conversationVersion: 8})).toBe(false)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(5)
  })

  it('does nothing for an event without versions (a client-owned turn, an older server)', () => {
    const id = seed([msg('u1')], 5)
    expect(adoptStreamVersion(id, undefined)).toBe(false)
    expect(adoptStreamVersion(id, {servedModel: 'x'})).toBe(false)
    expect(adoptStreamVersion(id, {conversationVersion: 9})).toBe(false)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(5)
  })

  it('does nothing for a thread whose version is unknown', () => {
    const id = seed([msg('u1')])
    useChatStore.setState(s => ({historyStatus: {...s.historyStatus, [id]: {state: 'loaded', token: 1}}}))
    expect(adoptStreamVersion(id, {conversationBase: 0, conversationVersion: 3})).toBe(false)
    expect(useChatStore.getState().getConversationVersion(id)).toBeUndefined()
  })

  it('never goes backwards', () => {
    const id = seed([msg('u1')], 6)
    expect(adoptStreamVersion(id, {conversationBase: 6, conversationVersion: 4})).toBe(false)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(6)
  })
})

describe('isPersistable (R-approvals-4)', () => {
  it('keeps a failed turn that left something behind, drops a bare error bubble', () => {
    const failed = (extra: Partial<ChatMessage>) => ({
      ...msg('f', 'assistant'),
      finishReason: 'error' as const,
      ...extra,
    })
    expect(isPersistable(failed({content: '*Error: provider down*'}))).toBe(false)
    expect(isPersistable(failed({content: 'The flow starts by\n\n---\n*Error: provider down*'}))).toBe(true)
    expect(isPersistable(failed({content: '*Error: x*', toolCalls: [{name: 'list_findings', ok: true}]}))).toBe(true)
    expect(
      isPersistable(
        failed({
          content: '*Error: x*',
          fixProposals: [
            {proposalId: 'p', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: '', status: 'applied'},
          ],
        }),
      ),
    ).toBe(true)
  })
})

// R-security-3: a key that holds another user's conversation is refused (409 CONVERSATION_KEY_TAKEN). No reload makes
// such a save succeed: the thread moves to a new key, keeping its messages, and is saved there.
describe('a save refused because another user holds the key', () => {
  const keyTaken = () => Object.assign(new Error('taken'), {status: 409, code: 'CONVERSATION_KEY_TAKEN'})

  it('moves the thread to a new key and saves its messages there, from version 0', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 3)
    save.mockRejectedValueOnce(keyTaken()).mockResolvedValueOnce({version: 1})

    await saveThreadConversation(id, 'flow1', id)

    const st = useChatStore.getState()
    expect(st.threads.some(t => t.id === id)).toBe(false)
    const moved = st.threads.find(t => t.flowId === 'flow1')!
    expect(moved.id).not.toBe(id)
    expect(save).toHaveBeenLastCalledWith('flow1', moved.id, [msg('u1'), msg('a1', 'assistant')], 0)
    expect(st.getConversationVersion(moved.id)).toBe(1)
    expect(st.getMessages(moved.id).map(m => m.id)).toEqual(['u1', 'a1'])
  })

  it('waits for a running stream to end before moving the thread, and saves no failure meanwhile', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant')], 3)
    useChatStore.getState().startStream(id, 's2', 'm2')
    save.mockRejectedValueOnce(keyTaken()).mockResolvedValueOnce({version: 1})

    const saving = saveThreadConversation(id, 'flow1', id)
    await Promise.resolve()
    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(true)
    useChatStore.getState().endStream(id)
    await expect(saving).resolves.toBeUndefined()

    const moved = useChatStore.getState().threads.find(t => t.flowId === 'flow1')!
    expect(moved.id).not.toBe(id)
    expect(save).toHaveBeenLastCalledWith('flow1', moved.id, [msg('u1'), msg('a1', 'assistant')], 0)
  })

  it('stores a compaction under the new key too', async () => {
    const id = seed([msg('u1'), msg('a1', 'assistant'), msg('u2')], 3)
    save.mockRejectedValueOnce(keyTaken()).mockResolvedValueOnce({version: 1})

    await expect(replaceThreadConversation(id, 'flow1', id, [msg('u2')])).resolves.toBe(true)

    const moved = useChatStore.getState().threads.find(t => t.flowId === 'flow1')!
    expect(moved.id).not.toBe(id)
    expect(save).toHaveBeenLastCalledWith('flow1', moved.id, [msg('u2')], 0)
  })
})
