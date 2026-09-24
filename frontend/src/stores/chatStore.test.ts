import {describe, it, expect, beforeEach, afterEach, vi} from 'vitest'
import {useChatStore, MAX_CONCURRENT_STREAMS} from './chatStore'
import {registerCurrentUserResolver} from './storeRegistry'
import type {ChatMessage} from '@/types'

function makeMessage(id: string, role: 'user' | 'assistant', content = 'hello'): ChatMessage {
  return {id, role, content, timestamp: new Date().toISOString()} as ChatMessage
}

beforeEach(() => {
  // Reset store to initial state before each test
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    selectedProvider: 'claude',
    drafts: {},
  })
})

// ---- thread CRUD ----

describe('createThread', () => {
  it('creates a thread and sets it as active', () => {
    const id = useChatStore.getState().createThread('flow1')
    const state = useChatStore.getState()
    expect(state.activeThreadId).toBe(id)
    expect(state.threads).toHaveLength(1)
    expect(state.threads[0].flowId).toBe('flow1')
  })

  it('returns a unique id each call', () => {
    const id1 = useChatStore.getState().createThread('flow1')
    const id2 = useChatStore.getState().createThread('flow1')
    expect(id1).not.toBe(id2)
  })

  it('newly created thread has empty title', () => {
    const id = useChatStore.getState().createThread('flow1')
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title).toBe('')
  })
})

describe('switchThread', () => {
  it('switches active thread to an existing thread', () => {
    const id1 = useChatStore.getState().createThread('flow1')
    const id2 = useChatStore.getState().createThread('flow1')
    useChatStore.getState().switchThread(id1)
    expect(useChatStore.getState().activeThreadId).toBe(id1)
    void id2
  })

  it('does not switch to a non-existent thread', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().switchThread('non-existent')
    expect(useChatStore.getState().activeThreadId).toBe(id)
  })
})

describe('closeThread', () => {
  it('removes the thread', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().closeThread(id)
    expect(useChatStore.getState().threads).toHaveLength(0)
  })

  it('sets active to the last remaining thread when active is closed', () => {
    const id1 = useChatStore.getState().createThread('flow1')
    const id2 = useChatStore.getState().createThread('flow1')
    useChatStore.getState().switchThread(id2)
    useChatStore.getState().closeThread(id2)
    expect(useChatStore.getState().activeThreadId).toBe(id1)
  })

  it('sets active to null when last thread is closed', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().closeThread(id)
    expect(useChatStore.getState().activeThreadId).toBeNull()
  })

  it('also removes the thread conversation', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user'))
    useChatStore.getState().closeThread(id)
    expect(useChatStore.getState().conversations.has(id)).toBe(false)
  })
})

describe('updateThread', () => {
  it('patches thread fields', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().updateThread(id, {title: 'My thread', contextBlockId: 'b1'})
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title).toBe('My thread')
    expect(thread.contextBlockId).toBe('b1')
  })
})

// ---- messages ----

describe('appendMessage', () => {
  it('adds message to thread conversation', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', 'Hello'))
    expect(useChatStore.getState().getMessages(id)).toHaveLength(1)
  })

  it('auto-sets thread title from first user message', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', 'What does this flow do?'))
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title).toBe('What does this flow do?')
  })

  it('does not overwrite existing title', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().updateThread(id, {title: 'Existing'})
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', 'New message'))
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title).toBe('Existing')
  })

  it('truncates title to 40 chars', () => {
    const id = useChatStore.getState().createThread('flow1')
    const long = 'A'.repeat(60)
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', long))
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title.length).toBe(40)
  })
})

describe('removeMessage', () => {
  it('removes message by id', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user'))
    useChatStore.getState().removeMessage(id, 'm1')
    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
  })
})

describe('clearThreadMessages', () => {
  it('clears all messages for a thread', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user'))
    useChatStore.getState().appendMessage(id, makeMessage('m2', 'assistant'))
    useChatStore.getState().clearThreadMessages(id)
    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
  })
})

// ---- compactThread ----

describe('compactThread', () => {
  it('keeps only the last N*2 messages', () => {
    const id = useChatStore.getState().createThread('flow1')
    for (let i = 0; i < 10; i++) {
      const role = i % 2 === 0 ? 'user' : 'assistant'
      useChatStore.getState().appendMessage(id, makeMessage(`m${i}`, role))
    }
    useChatStore.getState().compactThread(id, 2)
    expect(useChatStore.getState().getMessages(id)).toHaveLength(4)
  })

  it('does nothing when messages are within keepPairs*2', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user'))
    useChatStore.getState().appendMessage(id, makeMessage('m2', 'assistant'))
    useChatStore.getState().compactThread(id, 2)
    expect(useChatStore.getState().getMessages(id)).toHaveLength(2)
  })

  it('keeps the LAST messages (most recent)', () => {
    const id = useChatStore.getState().createThread('flow1')
    for (let i = 0; i < 6; i++) {
      const role = i % 2 === 0 ? 'user' : 'assistant'
      useChatStore.getState().appendMessage(id, makeMessage(`m${i}`, role))
    }
    useChatStore.getState().compactThread(id, 1)
    const msgs = useChatStore.getState().getMessages(id)
    expect(msgs).toHaveLength(2)
    expect(msgs[0].id).toBe('m4')
    expect(msgs[1].id).toBe('m5')
  })
})

// ---- streaming (per-thread) ----

describe('streaming', () => {
  it('startStream reserves a per-thread slot with thinking defaults', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    const slot = useChatStore.getState().streams['t1']
    expect(slot).toEqual({
      streamId: 'stream1',
      messageId: 'msg1',
      text: '',
      isThinking: true,
      tokens: 0,
      toolStatus: null,
      toolCalls: [],
      fixProposals: [],
    })
  })

  it('addToolCall appends a finished execution without mutating prior state', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    const before = useChatStore.getState().streams['t1']!
    useChatStore
      .getState()
      .addToolCall('t1', {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 5, summary: '3 matches'})
    useChatStore.getState().addToolCall('t1', {name: 'no_such', ok: false})
    const after = useChatStore.getState().streams['t1']!
    expect(after.toolCalls).toHaveLength(2)
    expect(after.toolCalls[0]).toMatchObject({name: 'search_flow', ok: true, durationMs: 5})
    expect(after.toolCalls[1]).toMatchObject({name: 'no_such', ok: false})
    expect(before.toolCalls).toHaveLength(0) // prior array untouched (immutable append)
  })

  it('addToolCall is a no-op for a thread with no slot', () => {
    useChatStore.getState().addToolCall('nope', {name: 'x', ok: true})
    expect(useChatStore.getState().streams['nope']).toBeUndefined()
  })

  it('updateStreamingMessage updates the slot text', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    useChatStore.getState().updateStreamingMessage('t1', 'Hello world')
    expect(useChatStore.getState().streams['t1']!.text).toBe('Hello world')
  })

  it('updateStreamingMessage is a no-op for a thread with no slot', () => {
    useChatStore.getState().updateStreamingMessage('nope', 'Hello world')
    expect(useChatStore.getState().streams['nope']).toBeUndefined()
  })

  it('endStream clears the slot', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    useChatStore.getState().endStream('t1')
    expect(useChatStore.getState().streams['t1']).toBeUndefined()
  })

  it('endStream is a no-op for a thread with no slot', () => {
    useChatStore.getState().endStream('nope')
    expect(useChatStore.getState().streams['nope']).toBeUndefined()
  })

  it('setStreamMeta patches only the allowed fields', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    useChatStore.getState().setStreamMeta('t1', {isThinking: false, tokens: 42, toolStatus: 'Searching flow'})
    const slot = useChatStore.getState().streams['t1']!
    expect(slot.isThinking).toBe(false)
    expect(slot.tokens).toBe(42)
    expect(slot.toolStatus).toBe('Searching flow')
    expect(slot.streamId).toBe('stream1') // unchanged
  })

  it('updateStream patches text AND meta in one atomic update', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    useChatStore.getState().updateStream('t1', {text: 'Hello', isThinking: false, tokens: 3})
    const slot = useChatStore.getState().streams['t1']!
    expect(slot.text).toBe('Hello')
    expect(slot.isThinking).toBe(false)
    expect(slot.tokens).toBe(3)
    expect(slot.streamId).toBe('stream1') // untouched fields preserved
  })

  it('updateStream is a no-op for a thread with no slot', () => {
    useChatStore.getState().updateStream('nope', {text: 'x', tokens: 1})
    expect(useChatStore.getState().streams['nope']).toBeUndefined()
  })

  it('several threads can stream concurrently', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().startStream('t2', 's2', 'm2')
    const state = useChatStore.getState()
    expect(Object.keys(state.streams)).toHaveLength(2)
    expect(state.streams['t1']!.streamId).toBe('s1')
    expect(state.streams['t2']!.streamId).toBe('s2')
  })

  it('one stream per thread: a second startStream in the same thread overwrites', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().startStream('t1', 's2', 'm2')
    const slot = useChatStore.getState().streams['t1']!
    expect(slot.streamId).toBe('s2')
    expect(slot.messageId).toBe('m2')
    expect(Object.keys(useChatStore.getState().streams)).toHaveLength(1)
  })
})

// ---- concurrency cap ----

describe('concurrency cap', () => {
  it('activeStreamCount reflects the number of streaming threads', () => {
    expect(useChatStore.getState().activeStreamCount()).toBe(0)
    useChatStore.getState().startStream('t1', 's1', 'm1')
    expect(useChatStore.getState().activeStreamCount()).toBe(1)
    useChatStore.getState().startStream('t2', 's2', 'm2')
    expect(useChatStore.getState().activeStreamCount()).toBe(2)
  })

  it(`canStartStream is false at MAX_CONCURRENT_STREAMS (${MAX_CONCURRENT_STREAMS})`, () => {
    expect(useChatStore.getState().canStartStream()).toBe(true)
    for (let i = 0; i < MAX_CONCURRENT_STREAMS; i++) {
      useChatStore.getState().startStream(`t${i}`, `s${i}`, `m${i}`)
    }
    expect(useChatStore.getState().activeStreamCount()).toBe(MAX_CONCURRENT_STREAMS)
    expect(useChatStore.getState().canStartStream()).toBe(false)
    // Freeing one slot re-enables starts.
    useChatStore.getState().endStream('t0')
    expect(useChatStore.getState().canStartStream()).toBe(true)
  })
})

// ---- closeThread drops in-flight stream ----

describe('closeThread stream cleanup', () => {
  it('closing a streaming thread drops its slot', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().closeThread('t1') // closeThread tolerates a non-created id
    expect(useChatStore.getState().streams['t1']).toBeUndefined()
  })

  it('closing one streaming thread leaves the others intact', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().startStream('t2', 's2', 'm2')
    useChatStore.getState().closeThread('t1')
    expect(useChatStore.getState().streams['t1']).toBeUndefined()
    expect(useChatStore.getState().streams['t2']).toBeDefined()
  })
})

// ---- multi-flow management ----

describe('getFlowThreads / clearFlowThreads', () => {
  it('getFlowThreads returns threads for the given flow', () => {
    useChatStore.getState().createThread('flow1')
    useChatStore.getState().createThread('flow1')
    useChatStore.getState().createThread('flow2')
    expect(useChatStore.getState().getFlowThreads('flow1')).toHaveLength(2)
    expect(useChatStore.getState().getFlowThreads('flow2')).toHaveLength(1)
  })

  it('clearFlowThreads removes all threads for a flow', () => {
    useChatStore.getState().createThread('flow1')
    useChatStore.getState().createThread('flow1')
    const otherId = useChatStore.getState().createThread('flow2')
    useChatStore.getState().clearFlowThreads('flow1')
    const state = useChatStore.getState()
    expect(state.threads).toHaveLength(1)
    expect(state.threads[0].id).toBe(otherId)
  })

  it('clearFlowThreads updates activeThreadId when active thread is cleared', () => {
    const id1 = useChatStore.getState().createThread('flow1')
    useChatStore.getState().switchThread(id1)
    useChatStore.getState().clearFlowThreads('flow1')
    expect(useChatStore.getState().activeThreadId).toBeNull()
  })
})

// ---- drafts ----

describe('setDraft', () => {
  it('stores and updates a per-thread draft', () => {
    useChatStore.getState().setDraft('t1', 'hello')
    expect(useChatStore.getState().drafts['t1']).toBe('hello')
    useChatStore.getState().setDraft('t1', 'hello world')
    expect(useChatStore.getState().drafts['t1']).toBe('hello world')
  })

  it('keeps drafts isolated per thread', () => {
    useChatStore.getState().setDraft('t1', 'a')
    useChatStore.getState().setDraft('t2', 'b')
    expect(useChatStore.getState().drafts).toEqual({t1: 'a', t2: 'b'})
  })

  it('prunes the key when the draft is emptied', () => {
    useChatStore.getState().setDraft('t1', 'a')
    useChatStore.getState().setDraft('t1', '')
    expect('t1' in useChatStore.getState().drafts).toBe(false)
  })

  it('closeThread drops the thread draft', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().setDraft(id, 'unsent')
    useChatStore.getState().closeThread(id)
    expect(id in useChatStore.getState().drafts).toBe(false)
  })

  // Regression: closeThread cleared conversations, streams and drafts but NOT
  // queuedByThread, so a queued follow-up outlived the thread it belonged to for
  // the rest of the session. clearFlowThreads clears all four and its comment
  // claims it is "mirroring closeThread" — which was precisely what closeThread
  // did not do.
  it('closeThread drops the queued follow-up', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().queueMessage(id, {text: 'follow-up', files: []})
    expect(id in useChatStore.getState().queuedByThread).toBe(true)

    useChatStore.getState().closeThread(id)
    expect(id in useChatStore.getState().queuedByThread).toBe(false)
  })

  it("closeThread leaves other threads' queued messages alone", () => {
    const keep = useChatStore.getState().createThread('flow1')
    const drop = useChatStore.getState().createThread('flow1')
    useChatStore.getState().queueMessage(keep, {text: 'keep me', files: []})
    useChatStore.getState().queueMessage(drop, {text: 'drop me', files: []})

    useChatStore.getState().closeThread(drop)

    expect(useChatStore.getState().queuedByThread[keep]?.text).toBe('keep me')
    expect(drop in useChatStore.getState().queuedByThread).toBe(false)
  })
})

describe('queued follow-up messages (U1.6)', () => {
  it('queue / take / clear semantics', () => {
    const st = useChatStore.getState()
    st.queueMessage('t1', {text: 'hello', files: [], excludeContext: true})
    expect(useChatStore.getState().queuedByThread['t1']?.text).toBe('hello')

    // take returns the message AND removes it (single-shot).
    const got = useChatStore.getState().takeQueuedMessage('t1')
    expect(got?.text).toBe('hello')
    expect(useChatStore.getState().takeQueuedMessage('t1')).toBeUndefined()

    // clear on an empty thread is a no-op.
    expect(() => useChatStore.getState().clearQueuedMessage('t1')).not.toThrow()

    // re-queue replaces (one per thread).
    st.queueMessage('t1', {text: 'first', files: []})
    st.queueMessage('t1', {text: 'second', files: []})
    expect(useChatStore.getState().takeQueuedMessage('t1')?.text).toBe('second')
  })
})

// ---- thread persistence (conversation keys must survive a reload) ----

describe('thread persistence', () => {
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
  // In the web app thread lists are stored per signed-in user.
  let userId: string | null = 'user-1'
  const KEY = (flowId: string, uid = 'user-1') => `baki:chatThreads:v2:${uid}:${flowId}`

  beforeEach(() => {
    for (const k of Object.keys(stored)) delete stored[k]
    vi.stubGlobal('localStorage', fakeStorage)
    userId = 'user-1'
    registerCurrentUserResolver(() => userId)
  })
  afterEach(() => {
    registerCurrentUserResolver(() => null)
  })

  it('restores a flow’s threads with the same ids so their conversations stay reachable', () => {
    const a = useChatStore.getState().createThread('flow1')
    const b = useChatStore.getState().createThread('flow1')
    useChatStore.getState().updateThread(b, {title: 'second', contextBlockId: 'blk-1', useTools: true})
    useChatStore.getState().createThread('other-flow')

    // Simulate a reload: the in-memory store is empty, localStorage is not.
    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})

    const ids = useChatStore.getState().hydrateThreads('flow1')
    expect(ids).toEqual([a, b])
    const state = useChatStore.getState()
    expect(state.activeThreadId).toBe(b)
    expect(state.threads.find(t => t.id === b)).toMatchObject({
      flowId: 'flow1',
      title: 'second',
      contextBlockId: 'blk-1',
      useTools: true,
    })
    // Other flows are untouched by a per-flow hydrate.
    expect(state.threads.some(t => t.flowId === 'other-flow')).toBe(false)
    // Conversations are NOT pre-populated: the restore effect loads them by key.
    expect(state.conversations.has(a)).toBe(false)
  })

  it('returns the existing ids without touching storage when threads are already loaded', () => {
    const a = useChatStore.getState().createThread('flow1')
    stored[KEY('flow1')] = JSON.stringify([{id: 'ffffffff-ffff-4fff-8fff-ffffffffffff', flowId: 'flow1'}])
    expect(useChatStore.getState().hydrateThreads('flow1')).toEqual([a])
    expect(useChatStore.getState().threads).toHaveLength(1)
  })

  it('drops closed threads from storage and ignores malformed entries', () => {
    const a = useChatStore.getState().createThread('flow1')
    const b = useChatStore.getState().createThread('flow1')
    useChatStore.getState().closeThread(a)
    stored[KEY('flow2')] = JSON.stringify([{id: 'not-a-uuid', flowId: 'flow2'}, 'garbage', null])

    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})
    expect(useChatStore.getState().hydrateThreads('flow1')).toEqual([b])
    expect(useChatStore.getState().hydrateThreads('flow2')).toEqual([])
  })

  it('removes the key when the last thread of a flow is cleared', () => {
    useChatStore.getState().createThread('flow1')
    expect(stored[KEY('flow1')]).toBeDefined()
    useChatStore.getState().clearFlowThreads('flow1')
    expect(stored[KEY('flow1')]).toBeUndefined()
  })

  // Shared browser: the next account to open the same flow must not get the
  // previous account's thread ids — the server lets any flow viewer read a
  // conversation by key, and the titles are the start of each first prompt.
  it("keeps each user's threads apart", () => {
    useChatStore.getState().createThread('flow1')
    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})

    userId = 'user-2'
    expect(useChatStore.getState().hydrateThreads('flow1')).toEqual([])
    expect(useChatStore.getState().threads).toHaveLength(0)
  })

  it('stores nothing while no one is signed in', () => {
    userId = null
    useChatStore.getState().createThread('flow1')
    expect(Object.keys(stored)).toHaveLength(0)
  })

  it('drops the unscoped legacy lists in the web app', () => {
    stored['baki:chatThreads:v1:flow1'] = JSON.stringify([
      {id: 'ffffffff-ffff-4fff-8fff-ffffffffffff', flowId: 'flow1'},
    ])
    // v1 lists record no owner, so they cannot be handed to whoever signs in.
    expect(useChatStore.getState().hydrateThreads('flow1')).toEqual([])
    expect(stored['baki:chatThreads:v1:flow1']).toBeUndefined()
  })

  it('keeps the single-user v1 key in the desktop app', () => {
    ;(window as unknown as Record<string, unknown>).__TAURI_INTERNALS__ = {}
    try {
      userId = null
      useChatStore.getState().createThread('flow1')
      expect(stored['baki:chatThreads:v1:flow1']).toBeDefined()
    } finally {
      delete (window as unknown as Record<string, unknown>).__TAURI_INTERNALS__
    }
  })
})

describe('history load state', () => {
  const m = (id: string): ChatMessage => ({id, role: 'user', content: id, timestamp: 'x'})

  it('treats a new thread as loaded (nothing to wait for)', () => {
    const id = useChatStore.getState().createThread('flow1')
    expect(useChatStore.getState().isHistorySynced(id)).toBe(true)
  })

  it('merges loaded history before messages sent while it loaded', () => {
    const id = useChatStore.getState().createThread('flow1')
    const token = useChatStore.getState().beginHistoryLoad(id)
    useChatStore.getState().appendMessage(id, m('local'))
    expect(useChatStore.getState().isHistorySynced(id)).toBe(false)

    expect(useChatStore.getState().finishHistoryLoad(id, token, [m('old1'), m('old2'), m('local')])).toBe(true)
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(x => x.id),
    ).toEqual(['old1', 'old2', 'local'])
    expect(useChatStore.getState().isHistorySynced(id)).toBe(true)
  })

  it('drops a load that resolves after the thread was cleared', () => {
    const id = useChatStore.getState().createThread('flow1')
    const token = useChatStore.getState().beginHistoryLoad(id)
    useChatStore.getState().clearThreadMessages(id)

    expect(useChatStore.getState().finishHistoryLoad(id, token, [m('resurrected')])).toBe(false)
    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
  })

  it('records a failed load, which blocks saves until a load succeeds', () => {
    const id = useChatStore.getState().createThread('flow1')
    const token = useChatStore.getState().beginHistoryLoad(id)
    useChatStore.getState().finishHistoryLoad(id, token, null)
    expect(useChatStore.getState().historyStatus[id]?.state).toBe('error')
    expect(useChatStore.getState().isHistorySynced(id)).toBe(false)
  })
})

describe('threadForFlow', () => {
  it("returns the flow's last active thread, else its newest", () => {
    const a = useChatStore.getState().createThread('flow1')
    const b = useChatStore.getState().createThread('flow1')
    useChatStore.getState().createThread('flow2')
    expect(useChatStore.getState().threadForFlow('flow1')).toBe(b)

    useChatStore.getState().switchThread(a)
    expect(useChatStore.getState().threadForFlow('flow1')).toBe(a)

    useChatStore.getState().closeThread(a)
    expect(useChatStore.getState().threadForFlow('flow1')).toBe(b)
    expect(useChatStore.getState().threadForFlow('nope')).toBeNull()
  })
})

// The thread that replaces a closed active thread must come from the SAME
// flow: falling back to any remaining thread showed another flow's
// conversation under the open one.
describe('closing the active thread picks a replacement from its own flow', () => {
  it('falls back to the newest remaining thread of that flow', () => {
    useChatStore.getState().createThread('X')
    const y1 = useChatStore.getState().createThread('Y')
    const y2 = useChatStore.getState().createThread('Y')
    useChatStore.getState().closeThread(y2)
    expect(useChatStore.getState().activeThreadId).toBe(y1)
  })

  it("leaves no thread active rather than another flow's when the flow has none left", () => {
    useChatStore.getState().createThread('X')
    const y = useChatStore.getState().createThread('Y')
    useChatStore.getState().closeThread(y)
    expect(useChatStore.getState().activeThreadId).toBeNull()
  })

  it('clearFlowThreads never promotes a thread of another flow', () => {
    useChatStore.getState().createThread('X')
    useChatStore.getState().createThread('Y')
    useChatStore.getState().clearFlowThreads('Y')
    expect(useChatStore.getState().activeThreadId).toBeNull()
  })
})

describe('finishHistoryLoad merge order', () => {
  it('keeps a local-only message next to the loaded message it followed', () => {
    const id = useChatStore.getState().createThread('flow1')
    const local = [
      makeMessage('u1', 'user'),
      {...makeMessage('err1', 'assistant'), finishReason: 'error'} as ChatMessage,
      makeMessage('u2', 'user'),
      makeMessage('a2', 'assistant'),
    ]
    for (const m of local) useChatStore.getState().appendMessage(id, m)
    const token = useChatStore.getState().beginHistoryLoad(id)
    useChatStore
      .getState()
      .finishHistoryLoad(id, token, [
        makeMessage('u1', 'user'),
        makeMessage('u2', 'user'),
        makeMessage('a2', 'assistant'),
      ])
    // Not [u1, u2, a2, err1]: the old error bubble must not become the
    // "last reply" a Retry acts on.
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(['u1', 'err1', 'u2', 'a2'])
  })

  it('puts a message sent while the load ran after everything stored', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('u9', 'user'))
    const token = useChatStore.getState().beginHistoryLoad(id)
    useChatStore.getState().finishHistoryLoad(id, token, [makeMessage('u1', 'user'), makeMessage('a1', 'assistant')])
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(['u1', 'a1', 'u9'])
  })
})

describe('clearThreadMessages / setThreadMessages', () => {
  it('clearing a thread closed meanwhile does not bring it back', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('u1', 'user'))
    useChatStore.getState().closeThread(id)
    useChatStore.getState().clearThreadMessages(id)
    expect(useChatStore.getState().conversations.has(id)).toBe(false)
    expect(useChatStore.getState().historyStatus[id]).toBeUndefined()
  })

  it('setThreadMessages replaces the list wholesale (and ignores closed threads)', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('u1', 'user'))
    useChatStore.getState().setThreadMessages(id, [makeMessage('u2', 'user'), makeMessage('a2', 'assistant')])
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(['u2', 'a2'])
    useChatStore.getState().closeThread(id)
    useChatStore.getState().setThreadMessages(id, [makeMessage('u3', 'user')])
    expect(useChatStore.getState().conversations.has(id)).toBe(false)
  })
})
