import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

vi.mock('@/api', () => ({
  chatApi: {
    listThreads: vi.fn(),
    setThreadTitle: vi.fn(),
    saveConversation: vi.fn(),
    getConversation: vi.fn(),
    clearConversation: vi.fn(),
  },
}))

const platform = vi.hoisted(() => ({tauri: false}))
vi.mock('@/platform/guards', async importOriginal => ({
  ...(await importOriginal<typeof import('@/platform/guards')>()),
  isTauri: () => platform.tauri,
}))

import {chatApi} from '@/api'
import {useChatStore, MAX_THREADS} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {registerCurrentUserResolver} from '@/stores/storeRegistry'
import {
  syncThreadIndex,
  rekeyThread,
  rekeyThreadWhenIdle,
  startTitleSync,
  settleThreadTitle,
  noteConversationCleared,
  resetThreadIndexForTest,
  TITLE_SYNC_DEBOUNCE_MS,
} from './threadIndex'
import type {ServerThread} from '@/api/chat'

const stored: Record<string, string> = {}
const fakeStorage = {
  getItem: (k: string) => stored[k] ?? null,
  setItem: (k: string, v: string) => {
    stored[k] = v
  },
  removeItem: (k: string) => {
    delete stored[k]
  },
  key: (i: number) => Object.keys(stored)[i] ?? null,
  get length() {
    return Object.keys(stored).length
  },
}
const KEY = (flowId: string) => `baki:chatThreads:v2:user-1:${flowId}`
const storedIds = (flowId: string) => (JSON.parse(stored[KEY(flowId)] ?? '[]') as {id: string}[]).map(t => t.id)
const initialChat = useChatStore.getState()
const initialSettings = useSettingsStore.getState()

function server(id: string, over: Partial<ServerThread> = {}): ServerThread {
  return {
    id,
    title: '',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-02T00:00:00Z',
    messageCount: 2,
    version: 3,
    ...over,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  for (const k of Object.keys(stored)) delete stored[k]
  vi.stubGlobal('localStorage', fakeStorage)
  registerCurrentUserResolver(() => 'user-1')
  useChatStore.setState(initialChat, true)
  platform.tauri = false
  resetThreadIndexForTest()
  vi.mocked(chatApi.setThreadTitle).mockResolvedValue({})
})
afterEach(() => {
  registerCurrentUserResolver(() => null)
  useSettingsStore.setState(initialSettings, true)
  vi.useRealTimers()
})

describe('syncThreadIndex (R-frontend-c26 c)', () => {
  it("brings in the server's threads this browser lacks, with their titles, and keeps local-only ones", async () => {
    const local = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().appendMessage(local, {id: 'm1', role: 'user', content: 'local', timestamp: 't'})
    vi.mocked(chatApi.listThreads).mockResolvedValue({
      items: [server('s1', {title: 'Retry loop'}), server('s2', {title: ''})],
    })

    await syncThreadIndex('flow-1')

    const threads = useChatStore.getState().getFlowThreads('flow-1')
    expect(threads.map(t => t.id).sort()).toEqual(['s1', 's2', local].sort())
    expect(threads.find(t => t.id === 's1')!.title).toBe('Retry loop')
    expect(storedIds('flow-1').sort()).toEqual(['s1', 's2', local].sort())
    // The thread the user is in stays the active one.
    expect(useChatStore.getState().activeThreadId).toBe(local)
  })

  it("shows the server's title on a thread this browser already has", async () => {
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(id, {title: 'old name'})
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server(id, {title: 'Renamed elsewhere'})]})

    await syncThreadIndex('flow-1')

    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('Renamed elsewhere')
  })

  // The composer keeps what is being typed to itself until the thread changes, and a staged block prompt or a picked
  // block lives on the active thread: a sync that switched threads could lose any of it, so it never does.
  it('never switches away from the thread the user is in, even a fresh one being typed into', async () => {
    const fresh = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server('newest'), server('older')]})

    await syncThreadIndex('flow-1')

    const st = useChatStore.getState()
    expect(st.activeThreadId).toBe(fresh)
    expect(st.threads.some(t => t.id === fresh)).toBe(true)
    expect(st.getFlowThreads('flow-1').map(t => t.id)).toEqual([fresh, 'older', 'newest'])
  })

  it('keeps a staged block prompt and its thread through a sync', async () => {
    const fresh = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(fresh, {contextBlockId: 'b1'})
    useChatStore.getState().setStagedPrompt({threadId: fresh, text: 'Fix this block'})
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server('newest')]})

    await syncThreadIndex('flow-1')

    const st = useChatStore.getState()
    expect(st.activeThreadId).toBe(fresh)
    expect(st.stagedPrompt).toEqual({threadId: fresh, text: 'Fix this block'})
    expect(st.threads.find(t => t.id === fresh)!.contextBlockId).toBe('b1')
  })

  // R-frontend-c26 (c) review: the cap counts every flow, so a flow with many stored threads would leave no room for
  // the next one. Threads a sync brought in that were never opened make way for another flow's: they stay listed
  // on the server and come back when their flow is opened again.
  it("fills a second flow by dropping the first flow's untouched synced threads", async () => {
    const many = Array.from({length: 60}, (_, i) => server(`a${i}`))
    vi.mocked(chatApi.listThreads).mockResolvedValueOnce({items: many})
    const freshA = useChatStore.getState().createThread('flowA')
    await syncThreadIndex('flowA')
    expect(useChatStore.getState().threads).toHaveLength(MAX_THREADS)

    const freshB = useChatStore.getState().createThread('flowB')
    vi.mocked(chatApi.listThreads).mockResolvedValueOnce({items: Array.from({length: 10}, (_, i) => server(`b${i}`))})
    await syncThreadIndex('flowB')

    const st = useChatStore.getState()
    expect(st.getFlowThreads('flowB').map(t => t.id)).toHaveLength(11)
    expect(st.getFlowThreads('flowB').map(t => t.id)).toContain(freshB)
    expect(st.threads).toHaveLength(MAX_THREADS)
    // flowA keeps the thread it was using, and its stored index matches what is in memory.
    expect(st.getFlowThreads('flowA').map(t => t.id)).toContain(freshA)
    expect(storedIds('flowA').sort()).toEqual(
      st
        .getFlowThreads('flowA')
        .map(t => t.id)
        .sort(),
    )
  })

  it('syncs nothing for a web deployment without accounts (no one signed in: no server index)', async () => {
    registerCurrentUserResolver(() => null)
    await syncThreadIndex('flow-1')
    expect(chatApi.listThreads).not.toHaveBeenCalled()
  })

  it('follows nextCursor up to the number of threads the client keeps, and adds no more than fit', async () => {
    for (let i = 0; i < MAX_THREADS - 3; i++) useChatStore.getState().createThread('other')
    vi.mocked(chatApi.listThreads)
      .mockResolvedValueOnce({items: [server('a'), server('b')], nextCursor: 'c1'})
      .mockResolvedValueOnce({items: [server('c'), server('d')], nextCursor: 'c2'})
      .mockResolvedValue({items: [server('e')]})

    await syncThreadIndex('flow-1')

    expect(vi.mocked(chatApi.listThreads).mock.calls.map(c => c[1])).toEqual([
      {limit: 50},
      {limit: 50, cursor: 'c1'},
      {limit: 50, cursor: 'c2'},
    ])
    expect(useChatStore.getState().threads).toHaveLength(MAX_THREADS)
    // The newest three, in the client's order (oldest first, as threads are created).
    expect(
      useChatStore
        .getState()
        .getFlowThreads('flow-1')
        .map(t => t.id),
    ).toEqual(['c', 'b', 'a'])
  })

  it('keeps a stored thread the server no longer lists: cleared elsewhere, it lives on in that other tab', async () => {
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().setConversationVersion(id, 4)
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server('s1')]})

    await syncThreadIndex('flow-1')

    expect(useChatStore.getState().threads.some(t => t.id === id)).toBe(true)
  })

  it('does nothing in the desktop app, nor with history saving off', async () => {
    platform.tauri = true
    await syncThreadIndex('flow-1')
    platform.tauri = false
    useSettingsStore.setState(s => ({
      settings: {...s.settings, ai: {...s.settings.ai, saveConversationHistory: false}},
    }))
    await syncThreadIndex('flow-1')
    expect(chatApi.listThreads).not.toHaveBeenCalled()
  })

  it('leaves the local index as it was when the list fails', async () => {
    const id = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.listThreads).mockRejectedValue(new Error('offline'))
    await syncThreadIndex('flow-1')
    expect(useChatStore.getState().threads.map(t => t.id)).toEqual([id])
  })
})

describe('rekeyThread (CONVERSATION_KEY_TAKEN)', () => {
  it('moves the thread, its messages, draft and place to a new key with nothing stored', () => {
    const old = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(old, {title: 'Mine'})
    useChatStore.getState().appendMessage(old, {id: 'm1', role: 'user', content: 'q', timestamp: 't'})
    useChatStore.getState().setDraft(old, 'half typed')
    useChatStore.getState().setConversationVersion(old, 6)

    const next = rekeyThread(old)!

    const st = useChatStore.getState()
    expect(next).not.toBe(old)
    expect(st.threads.some(t => t.id === old)).toBe(false)
    expect(st.threads.find(t => t.id === next)).toMatchObject({flowId: 'flow-1', title: 'Mine'})
    expect(st.getMessages(next).map(m => m.id)).toEqual(['m1'])
    expect(st.drafts[next]).toBe('half typed')
    expect(st.getConversationVersion(next)).toBe(0)
    expect(st.activeThreadId).toBe(next)
    expect(storedIds('flow-1')).toEqual([next])
  })

  it('refuses a thread with a running stream', () => {
    const old = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().startStream(old, 's1', 'm1')
    expect(rekeyThread(old)).toBeNull()
  })
})

describe('title sync (R-frontend-c26 c)', () => {
  it('sends a rename once, after the debounce, and adopts the version of a thread that stored nothing', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.setThreadTitle).mockResolvedValue({version: 1})

    useChatStore.getState().updateThread(id, {title: 'Retr'})
    useChatStore.getState().updateThread(id, {title: 'Retry loop'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(chatApi.setThreadTitle).toHaveBeenCalledTimes(1)
    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', id, 'Retry loop')
    expect(useChatStore.getState().getConversationVersion(id)).toBe(1)
    stop()
  })

  it('keeps the version of a stored thread: only a thread still at 0 adopts the one the rename names', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().setConversationVersion(id, 5)
    vi.mocked(chatApi.setThreadTitle).mockResolvedValue({version: 9})

    useChatStore.getState().updateThread(id, {title: 'Named'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(useChatStore.getState().getConversationVersion(id)).toBe(5)
    stop()
  })

  it('does not send back a title the sync changed', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server(id, {title: 'Named elsewhere'})]})

    await syncThreadIndex('flow-1')
    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('Named elsewhere')
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(chatApi.setThreadTitle).not.toHaveBeenCalled()
    stop()
  })

  it('sends nothing in the desktop app', async () => {
    vi.useFakeTimers()
    platform.tauri = true
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(id, {title: 'Named'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    expect(chatApi.setThreadTitle).not.toHaveBeenCalled()
    stop()
  })
})

describe('titles kept in the server index (review)', () => {
  it('sends the title of a thread moved to a new key', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const old = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(old, {title: 'Mine'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    vi.mocked(chatApi.setThreadTitle).mockClear()

    const next = rekeyThread(old)!
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', next, 'Mine')
    stop()
  })

  it('sends the title again after a clear deleted it with the conversation', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(id, {title: 'Mine'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    vi.mocked(chatApi.setThreadTitle).mockClear()

    noteConversationCleared(id)
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', id, 'Mine')
    stop()
  })

  it('sends a title the browser kept from before the index, when the server lists the thread unnamed', async () => {
    vi.useFakeTimers()
    const id = useChatStore.getState().createThread('flow-1')
    // As a title read back from localStorage before anything watched titles: no change for the watcher to see.
    useChatStore.setState(s => ({threads: s.threads.map(th => (th.id === id ? {...th, title: 'Old name'} : th))}))
    const stop = startTitleSync()
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server(id, {title: ''})]})

    await syncThreadIndex('flow-1')
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', id, 'Old name')
    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('Old name')
    stop()
  })

  it('a rename still on its way wins over a slower sync, and is sent', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server(id, {title: 'Server name'})]})

    useChatStore.getState().updateThread(id, {title: 'My new name'})
    await syncThreadIndex('flow-1')
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('My new name')
    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', id, 'My new name')
    stop()
  })

  it('a rename whose send failed wins over the sync, and is sent again', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    vi.mocked(chatApi.setThreadTitle).mockRejectedValueOnce(new Error('offline'))
    useChatStore.getState().updateThread(id, {title: 'My new name'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    vi.mocked(chatApi.setThreadTitle).mockClear()
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: [server(id, {title: 'Server name'})]})

    await syncThreadIndex('flow-1')
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('My new name')
    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow-1', id, 'My new name')
    stop()
  })
})

describe('closing a thread the index may hold (review)', () => {
  it('says a row may exist once a rename stored one, waiting for a rename still on its way', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    let answer: (v: {version?: number}) => void = () => {}
    vi.mocked(chatApi.setThreadTitle).mockImplementationOnce(() => new Promise(r => (answer = r)))
    useChatStore.getState().updateThread(id, {title: 'Named'})
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)

    const settled = settleThreadTitle(id)
    answer({version: 1})
    await expect(settled).resolves.toBe(true)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(1)
    stop()
  })

  it('cancels a rename not sent yet: no row is made for a thread about to close', async () => {
    vi.useFakeTimers()
    const stop = startTitleSync()
    const id = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().updateThread(id, {title: 'Named'})

    expect(await settleThreadTitle(id)).toBe(false)
    await vi.advanceTimersByTimeAsync(TITLE_SYNC_DEBOUNCE_MS)
    expect(chatApi.setThreadTitle).not.toHaveBeenCalled()
    stop()
  })
})

describe('rekeyThreadWhenIdle (review)', () => {
  it('waits for a running stream to end, then moves the thread', async () => {
    const old = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().startStream(old, 's1', 'm1')

    const moved = rekeyThreadWhenIdle(old)
    await Promise.resolve()
    expect(useChatStore.getState().threads.some(th => th.id === old)).toBe(true)

    useChatStore.getState().endStream(old)
    const next = await moved
    expect(next).not.toBeNull()
    expect(useChatStore.getState().threads.some(th => th.id === next)).toBe(true)
    expect(useChatStore.getState().threads.some(th => th.id === old)).toBe(false)
  })

  it('gives up when the thread closes while it waits', async () => {
    const old = useChatStore.getState().createThread('flow-1')
    useChatStore.getState().startStream(old, 's1', 'm1')
    const moved = rekeyThreadWhenIdle(old)
    useChatStore.getState().closeThread(old)
    await expect(moved).resolves.toBeNull()
  })
})
