import {describe, it, expect, beforeEach, afterEach, vi} from 'vitest'
import {
  useChatStore,
  MAX_CONCURRENT_STREAMS,
  MAX_THREADS,
  threadModelOf,
  autoTitle,
  AUTO_TITLE_MAX,
  MAX_QUEUED_MESSAGES,
} from './chatStore'
import {useSettingsStore} from './settingsStore'
import {chatApi} from '@/api'
import {registerCurrentUserResolver} from './storeRegistry'
import type {ChatMessage} from '@/types'

function makeMessage(id: string, role: 'user' | 'assistant', content = 'hello'): ChatMessage {
  return {id, role, content, timestamp: new Date().toISOString()} as ChatMessage
}

beforeEach(() => {
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    drafts: {},
  })
})

describe('createThread', () => {
  it('creates a thread and sets it as active', () => {
    const id = useChatStore.getState().createThread('flow1')
    const state = useChatStore.getState()
    expect(state.activeThreadId).toBe(id)
    expect(state.threads).toHaveLength(1)
    expect(state.threads[0]!.flowId).toBe('flow1')
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

  // R-frontend-19: one rule (autoTitle) — whitespace runs collapse to one space, trimmed, then 50 characters.
  it('titles a 60-character first message with newlines by the one autoTitle rule', () => {
    const id = useChatStore.getState().createThread('flow1')
    const text = `  ${'A'.repeat(20)}\n\n${'B'.repeat(20)}\n${'C'.repeat(16)}  `
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', text))
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(thread.title).toBe(`${'A'.repeat(20)} ${'B'.repeat(20)} ${'C'.repeat(8)}`)
    expect(thread.title).toHaveLength(AUTO_TITLE_MAX)
    expect(thread.title).toBe(autoTitle(text))
  })

  it('leaves a whitespace-only first message untitled', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().appendMessage(id, makeMessage('m1', 'user', ' \n\t '))
    expect(useChatStore.getState().threads.find(t => t.id === id)!.title).toBe('')
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
    expect(msgs[0]!.id).toBe('m4')
    expect(msgs[1]!.id).toBe('m5')
  })
})

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
      approvals: [],
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
    expect(before.toolCalls).toHaveLength(0)
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
    expect(slot.streamId).toBe('stream1')
  })

  it('updateStream patches text AND meta in one atomic update', () => {
    useChatStore.getState().startStream('t1', 'stream1', 'msg1')
    useChatStore.getState().updateStream('t1', {text: 'Hello', isThinking: false, tokens: 3})
    const slot = useChatStore.getState().streams['t1']!
    expect(slot.text).toBe('Hello')
    expect(slot.isThinking).toBe(false)
    expect(slot.tokens).toBe(3)
    expect(slot.streamId).toBe('stream1')
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
    useChatStore.getState().endStream('t0')
    expect(useChatStore.getState().canStartStream()).toBe(true)
  })
})

describe('closeThread stream cleanup', () => {
  it('closing a streaming thread drops its slot', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().closeThread('t1')
    expect(useChatStore.getState().streams['t1']).toBeUndefined()
  })

  it('only removes state: the stream engine owns the cancel (R-frontend-17)', () => {
    const cancel = vi.spyOn(chatApi, 'cancelStream').mockResolvedValue(undefined)
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().closeThread('t1')
    expect(cancel).not.toHaveBeenCalled()
    cancel.mockRestore()
  })

  it('closing one streaming thread leaves the others intact', () => {
    useChatStore.getState().startStream('t1', 's1', 'm1')
    useChatStore.getState().startStream('t2', 's2', 'm2')
    useChatStore.getState().closeThread('t1')
    expect(useChatStore.getState().streams['t1']).toBeUndefined()
    expect(useChatStore.getState().streams['t2']).toBeDefined()
  })
})

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
    expect(state.threads[0]!.id).toBe(otherId)
  })

  it('clearFlowThreads updates activeThreadId when active thread is cleared', () => {
    const id1 = useChatStore.getState().createThread('flow1')
    useChatStore.getState().switchThread(id1)
    useChatStore.getState().clearFlowThreads('flow1')
    expect(useChatStore.getState().activeThreadId).toBeNull()
  })
})

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

    expect(useChatStore.getState().queuedByThread[keep]?.map(q => q.text)).toEqual(['keep me'])
    expect(drop in useChatStore.getState().queuedByThread).toBe(false)
  })
})

describe('queued follow-up messages (U1.6)', () => {
  beforeEach(() => {
    useChatStore.setState({queuedByThread: {}})
  })

  it('queue / take / clear semantics', () => {
    const st = useChatStore.getState()
    st.queueMessage('t1', {text: 'hello', files: [], excludeContext: true})
    expect(useChatStore.getState().queuedByThread['t1']?.[0]?.text).toBe('hello')

    const got = useChatStore.getState().takeQueuedMessage('t1')
    expect(got?.text).toBe('hello')
    expect(useChatStore.getState().takeQueuedMessage('t1')).toBeUndefined()
    expect('t1' in useChatStore.getState().queuedByThread).toBe(false)

    expect(() => useChatStore.getState().clearQueuedMessage('t1')).not.toThrow()
  })

  // R-frontend-13: a second follow-up used to overwrite the first.
  it('keeps two queued follow-ups in order and hands them out FIFO', () => {
    const st = useChatStore.getState()
    expect(st.queueMessage('t1', {text: 'first', files: []})).toBe(true)
    expect(st.queueMessage('t1', {text: 'second', files: []})).toBe(true)
    expect(useChatStore.getState().queuedByThread['t1']?.map(q => q.text)).toEqual(['first', 'second'])

    expect(useChatStore.getState().takeQueuedMessage('t1')?.text).toBe('first')
    expect(useChatStore.getState().takeQueuedMessage('t1')?.text).toBe('second')
    expect(useChatStore.getState().takeQueuedMessage('t1')).toBeUndefined()
  })

  it(`refuses a follow-up past the cap of ${MAX_QUEUED_MESSAGES} and keeps the queue as it was`, () => {
    const st = useChatStore.getState()
    for (let i = 0; i < MAX_QUEUED_MESSAGES; i++) expect(st.queueMessage('t1', {text: `m${i}`, files: []})).toBe(true)
    expect(st.queueMessage('t1', {text: 'one too many', files: []})).toBe(false)
    const texts = useChatStore.getState().queuedByThread['t1']?.map(q => q.text)
    expect(texts).toHaveLength(MAX_QUEUED_MESSAGES)
    expect(texts).not.toContain('one too many')
  })

  it('removes one queued follow-up by id and leaves the rest', () => {
    const st = useChatStore.getState()
    st.queueMessage('t1', {text: 'first', files: []})
    st.queueMessage('t1', {text: 'second', files: []})
    const [first] = useChatStore.getState().queuedByThread['t1']!
    useChatStore.getState().removeQueuedMessage('t1', first!.id)
    expect(useChatStore.getState().queuedByThread['t1']?.map(q => q.text)).toEqual(['second'])
    useChatStore.getState().removeQueuedMessage('t1', useChatStore.getState().queuedByThread['t1']![0]!.id)
    expect('t1' in useChatStore.getState().queuedByThread).toBe(false)
  })

  // Review of R-frontend-13: after a failed or stopped turn the queue holds instead of firing the next message.
  describe('paused queue', () => {
    beforeEach(() => {
      useChatStore.setState({queuedByThread: {}, queuePausedByThread: {}})
    })

    it('pauses a thread with queued follow-ups, keeps them, and resumes', () => {
      const st = useChatStore.getState()
      st.queueMessage('t1', {text: 'first', files: []})
      st.pauseQueue('t1', 'error')
      expect(useChatStore.getState().queuePausedByThread['t1']).toBe('error')
      expect(useChatStore.getState().queuedByThread['t1']?.map(q => q.text)).toEqual(['first'])
      useChatStore.getState().resumeQueue('t1')
      expect('t1' in useChatStore.getState().queuePausedByThread).toBe(false)
    })

    it('does not pause a thread with nothing queued', () => {
      useChatStore.getState().pauseQueue('t1', 'stopped')
      expect('t1' in useChatStore.getState().queuePausedByThread).toBe(false)
    })

    it('forgets the pause once the queue is empty', () => {
      const st = useChatStore.getState()
      st.queueMessage('t1', {text: 'only', files: []})
      st.pauseQueue('t1', 'stopped')
      useChatStore.getState().removeQueuedMessage('t1', useChatStore.getState().queuedByThread['t1']![0]!.id)
      expect('t1' in useChatStore.getState().queuePausedByThread).toBe(false)
    })

    it('puts a message back at the head, even past the cap', () => {
      const st = useChatStore.getState()
      for (let i = 0; i < MAX_QUEUED_MESSAGES; i++) st.queueMessage('t1', {text: `m${i}`, files: []})
      const head = useChatStore.getState().takeQueuedMessage('t1')!
      useChatStore.getState().queueMessage('t1', {text: 'late', files: []})
      useChatStore.getState().requeueMessage('t1', head)
      const texts = useChatStore.getState().queuedByThread['t1']!.map(q => q.text)
      expect(texts[0]).toBe('m0')
      expect(texts).toHaveLength(MAX_QUEUED_MESSAGES + 1)
    })
  })
})

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
    expect(state.threads.some(t => t.flowId === 'other-flow')).toBe(false)
    expect(state.conversations.has(a)).toBe(false)
  })

  // R-frontend-6: tools are on by default, so a thread's "off" must survive a reload, not read back as "no choice".
  it('keeps a thread’s useTools false across a reload, and leaves an undecided thread undecided', () => {
    const off = useChatStore.getState().createThread('flow1')
    const undecided = useChatStore.getState().createThread('flow1')
    useChatStore.getState().updateThread(off, {useTools: false})

    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})
    useChatStore.getState().hydrateThreads('flow1')

    const threads = useChatStore.getState().threads
    expect(threads.find(t => t.id === off)?.useTools).toBe(false)
    expect(threads.find(t => t.id === undecided)?.useTools).toBeUndefined()
  })

  it('restores the lost-block notice of a cleared thread, as a string only', () => {
    const a = useChatStore.getState().createThread('flow1')
    useChatStore.getState().updateThread(a, {contextBlockId: null, contextBlockLost: 'Invoke Url'})
    stored[KEY('flow2')] = JSON.stringify([
      {id: 'ffffffff-ffff-4fff-8fff-ffffffffffff', flowId: 'flow2', contextBlockLost: 7},
    ])

    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})

    useChatStore.getState().hydrateThreads('flow1')
    useChatStore.getState().hydrateThreads('flow2')
    const threads = useChatStore.getState().threads
    expect(threads.find(t => t.id === a)?.contextBlockLost).toBe('Invoke Url')
    expect(threads.find(t => t.flowId === 'flow2')?.contextBlockLost).toBeUndefined()
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

describe('thread model pins', () => {
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
  const reload = () => useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map()})

  beforeEach(() => {
    for (const k of Object.keys(stored)) delete stored[k]
    vi.stubGlobal('localStorage', fakeStorage)
    registerCurrentUserResolver(() => 'user-1')
    useChatStore.setState({modelRefusals: {}, modelPickerRequest: 0})
  })
  afterEach(() => {
    registerCurrentUserResolver(() => null)
  })

  it('a new thread starts unpinned (it follows the Default for new chats)', () => {
    const id = useChatStore.getState().createThread('flow1')
    const thread = useChatStore.getState().threads.find(t => t.id === id)!
    expect(threadModelOf(thread)).toBeNull()
  })

  it('pins a thread and restores the pin after a reload', () => {
    const a = useChatStore.getState().createThread('flow1')
    const b = useChatStore.getState().createThread('flow1')
    useChatStore
      .getState()
      .setThreadModel(a, {provider: 'azure-foundry', model: 'm/docs000000', modelLabel: 'Docs writer'})
    reload()
    useChatStore.getState().hydrateThreads('flow1')
    const threads = useChatStore.getState().threads
    expect(threadModelOf(threads.find(t => t.id === a)!)).toEqual({
      provider: 'azure-foundry',
      model: 'm/docs000000',
      modelLabel: 'Docs writer',
    })
    expect(threadModelOf(threads.find(t => t.id === b)!)).toBeNull()
  })

  it('loads a thread from before per-thread memory, or with a bad pin, unpinned', () => {
    const base = {flowId: 'flow1', title: 't', createdAt: '2026-09-30T00:00:00Z'}
    stored[KEY('flow1')] = JSON.stringify([
      {...base, id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'},
      {...base, id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', provider: 7, model: 'x'},
      {...base, id: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', provider: 'claude', model: ''},
      {...base, id: 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', provider: 'claude', model: 'x'.repeat(300)},
      {
        ...base,
        id: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
        provider: 'claude',
        model: 'claude-opus-4-1',
        modelLabel: 7,
      },
    ])
    useChatStore.getState().hydrateThreads('flow1')
    const pins = useChatStore.getState().threads.map(threadModelOf)
    expect(pins.slice(0, 4)).toEqual([null, null, null, null])
    expect(pins[4]).toEqual({provider: 'claude', model: 'claude-opus-4-1'})
  })

  it('setThreadModel is a no-op for an unchanged or incomplete pin, and null unpins', () => {
    const id = useChatStore.getState().createThread('flow1')
    const st = useChatStore.getState()
    st.setThreadModel(id, {provider: 'claude', model: 'claude-opus-4-1', modelLabel: 'Opus'})
    const before = useChatStore.getState().threads
    st.setThreadModel(id, {provider: 'claude', model: 'claude-opus-4-1', modelLabel: 'Opus'})
    expect(useChatStore.getState().threads).toBe(before)
    st.setThreadModel(id, {provider: '', model: 'x'})
    st.setThreadModel(id, {provider: 'claude', model: ''})
    expect(useChatStore.getState().threads).toBe(before)
    st.setThreadModel(id, null)
    expect(threadModelOf(useChatStore.getState().threads[0]!)).toBeNull()
  })

  it('never stores an id as a pin’s label, and drops one stored before', () => {
    const id = useChatStore.getState().createThread('flow1')
    const st = useChatStore.getState()
    st.setThreadModel(id, {provider: 'azure-foundry', model: 'm/docs000000', modelLabel: 'm/docs000000'})
    expect(threadModelOf(useChatStore.getState().threads[0]!)).toEqual({
      provider: 'azure-foundry',
      model: 'm/docs000000',
    })
    st.setThreadModel(id, {provider: 'claude', model: 'claude-custom', modelLabel: 'claude-custom'})
    expect(threadModelOf(useChatStore.getState().threads[0]!)).toEqual({provider: 'claude', model: 'claude-custom'})

    const base = {flowId: 'flow1', title: 't', createdAt: '2026-09-30T00:00:00Z'}
    stored[KEY('flow2')] = JSON.stringify([
      {
        ...base,
        flowId: 'flow2',
        id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
        provider: 'azure-foundry',
        model: 'm/docs000000',
        modelLabel: 'm/docs000000',
      },
    ])
    useChatStore.getState().hydrateThreads('flow2')
    const restored = useChatStore.getState().threads.find(t => t.flowId === 'flow2')!
    expect(threadModelOf(restored)).toEqual({provider: 'azure-foundry', model: 'm/docs000000'})
  })

  it('keeps a thread’s in-flight turn across a reload, validated', () => {
    const a = useChatStore.getState().createThread('flow1')
    const turn = {
      streamId: '11111111-1111-4111-8111-111111111111',
      messageId: '22222222-2222-4222-8222-222222222222',
      provider: 'azure-foundry' as const,
      model: 'm/gpt4100000',
      modelLabel: 'GPT-4.1',
    }
    useChatStore.getState().updateThread(a, {inFlight: turn})
    reload()
    useChatStore.getState().hydrateThreads('flow1')
    expect(useChatStore.getState().threads.find(t => t.id === a)?.inFlight).toEqual(turn)

    const base = {flowId: 'flow3', title: 't', createdAt: '2026-09-30T00:00:00Z'}
    stored[KEY('flow3')] = JSON.stringify([
      {...base, id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', inFlight: {...turn, streamId: 'not-a-uuid'}},
      {...base, id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', inFlight: {...turn, provider: 7}},
      {...base, id: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', inFlight: {...turn, model: 7}},
      {...base, id: 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', inFlight: 'x'},
      {
        ...base,
        id: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
        inFlight: {...turn, messageId: 'x', modelLabel: 'm/gpt4100000', model: ''},
      },
    ])
    useChatStore.getState().hydrateThreads('flow3')
    const records = useChatStore
      .getState()
      .threads.filter(t => t.flowId === 'flow3')
      .map(t => t.inFlight)
    expect(records).toEqual([
      undefined,
      undefined,
      undefined,
      undefined,
      {streamId: turn.streamId, provider: 'azure-foundry', model: ''},
    ])
  })

  it('ends the in-flight record with its stream — and only its own', () => {
    const a = useChatStore.getState().createThread('flow1')
    const turn = {streamId: 'stream-1', provider: 'claude' as const, model: 'claude-opus-4-1'}
    useChatStore.getState().startStream(a, 'stream-2', 'msg-2')
    useChatStore.getState().updateThread(a, {inFlight: turn})
    useChatStore.getState().endStream(a)
    expect(useChatStore.getState().threads.find(t => t.id === a)?.inFlight).toEqual(turn)

    useChatStore.getState().startStream(a, 'stream-1', 'msg-1')
    useChatStore.getState().endStream(a)
    expect(useChatStore.getState().threads.find(t => t.id === a)?.inFlight).toBeUndefined()
    const storedThread = (JSON.parse(stored[KEY('flow1')]!) as {id: string}[]).find(t => t.id === a)
    expect(storedThread).toBeDefined()
    expect(storedThread).not.toHaveProperty('inFlight')
  })

  it('drops a thread’s refusal record with the thread, and asks the picker to open', () => {
    const a = useChatStore.getState().createThread('flow1')
    const b = useChatStore.getState().createThread('flow1')
    const refusal = {provider: 'azure-foundry' as const, model: 'm/docs000000', epoch: 1}
    useChatStore.getState().setModelRefusal(a, refusal)
    useChatStore.getState().setModelRefusal(b, refusal)
    useChatStore.getState().closeThread(a)
    expect(Object.keys(useChatStore.getState().modelRefusals)).toEqual([b])
    useChatStore.getState().setModelRefusal(b, null)
    expect(useChatStore.getState().modelRefusals).toEqual({})

    useChatStore.getState().openModelPicker()
    useChatStore.getState().openModelPicker()
    expect(useChatStore.getState().modelPickerRequest).toBe(2)
  })
})

describe('MAX_THREADS eviction (R-frontend-14, R-frontend-c26)', () => {
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
  const initialSettings = useSettingsStore.getState()

  beforeEach(() => {
    for (const k of Object.keys(stored)) delete stored[k]
    vi.stubGlobal('localStorage', fakeStorage)
    registerCurrentUserResolver(() => 'user-1')
    vi.spyOn(chatApi, 'clearConversation').mockResolvedValue(undefined)
  })
  afterEach(() => {
    registerCurrentUserResolver(() => null)
    useSettingsStore.setState(initialSettings, true)
    vi.restoreAllMocks()
    delete (window as unknown as Record<string, unknown>).__TAURI_INTERNALS__
  })

  // The desktop app keeps no server thread index: an evicted thread no list brings back is deleted with it.
  const asDesktop = () => {
    ;(window as unknown as Record<string, unknown>).__TAURI_INTERNALS__ = {}
  }

  function createMany(flowId: string, n: number): string[] {
    const ids: string[] = []
    for (let i = 0; i < n; i++) ids.push(useChatStore.getState().createThread(flowId))
    return ids
  }

  function giveHistory(threadId: string, version: number) {
    useChatStore.getState().appendMessage(threadId, makeMessage(`m-${threadId}`, 'user'))
    useChatStore.getState().setConversationVersion(threadId, version)
  }

  it("evicts the oldest thread of the creating flow, never another flow's while one qualifies", () => {
    const [other] = createMany('flowA', 1)
    const flowB = createMany('flowB', MAX_THREADS - 1)

    useChatStore.getState().createThread('flowB')

    const ids = useChatStore.getState().threads.map(t => t.id)
    expect(ids).toHaveLength(MAX_THREADS)
    expect(ids).toContain(other)
    expect(ids).not.toContain(flowB[0])
    expect(storedIds('flowB')).not.toContain(flowB[0])
  })

  // R-frontend-c26 (c): where the server lists the user's threads, an evicted thread stays reachable through that
  // list (the next sync brings it back), so eviction only drops it from this client and deletes nothing.
  it('with a server thread index, drops the victim locally and keeps its conversation', async () => {
    const flowA = createMany('flowA', MAX_THREADS)
    giveHistory(flowA[0]!, 2)

    useChatStore.getState().createThread('flowB')

    expect(useChatStore.getState().threads.some(t => t.id === flowA[0])).toBe(false)
    expect(storedIds('flowA')).not.toContain(flowA[0])
    await vi.dynamicImportSettled()
    await new Promise(r => setTimeout(r, 0))
    expect(chatApi.clearConversation).not.toHaveBeenCalled()
  })

  it('a web deployment without accounts keeps no server index either: the evicted conversation is cleared', async () => {
    registerCurrentUserResolver(() => null)
    createMany('flowA', 1)
    const flowB = createMany('flowB', MAX_THREADS - 1)
    giveHistory(flowB[0]!, 4)

    useChatStore.getState().createThread('flowB')

    await vi.waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalledWith('flowB', flowB[0], 4))
  })

  it("in the desktop app, clears the evicted thread's stored conversation on the version it holds", async () => {
    asDesktop()
    createMany('flowA', 1)
    const flowB = createMany('flowB', MAX_THREADS - 1)
    giveHistory(flowB[0]!, 4)

    useChatStore.getState().createThread('flowB')

    await vi.waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalledTimes(1))
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flowB', flowB[0], 4)
  })

  it("rewrites another flow's index when its thread is the victim", () => {
    const flowA = createMany('flowA', MAX_THREADS)
    expect(storedIds('flowA')).toContain(flowA[0])

    useChatStore.getState().createThread('flowB')

    expect(useChatStore.getState().threads.some(t => t.id === flowA[0])).toBe(false)
    expect(storedIds('flowA')).not.toContain(flowA[0])
    expect(storedIds('flowA')).toHaveLength(MAX_THREADS - 1)
  })

  it("in the desktop app, clears another flow's victim too", async () => {
    asDesktop()
    const flowA = createMany('flowA', MAX_THREADS)
    giveHistory(flowA[0]!, 2)

    useChatStore.getState().createThread('flowB')

    await vi.waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalledWith('flowA', flowA[0], 2))
  })

  it('leaves a victim whose history this client never loaded: no version, so no clear that could wipe a live one', async () => {
    asDesktop()
    const flowB = createMany('flowB', MAX_THREADS)
    // As a hydrated thread that was never opened: stored messages may exist, but no version is held.
    useChatStore.setState(s => {
      const historyStatus = {...s.historyStatus}
      delete historyStatus[flowB[0]!]
      return {historyStatus}
    })

    useChatStore.getState().createThread('flowB')

    expect(useChatStore.getState().threads.some(t => t.id === flowB[0])).toBe(false)
    await vi.dynamicImportSettled()
    await new Promise(r => setTimeout(r, 0))
    expect(chatApi.clearConversation).not.toHaveBeenCalled()
  })

  it('makes no server call for a victim with nothing stored, or with history saving off', async () => {
    asDesktop()
    createMany('flowB', MAX_THREADS)
    useChatStore.getState().createThread('flowB')
    expect(chatApi.clearConversation).not.toHaveBeenCalled()

    useSettingsStore.setState(s => ({
      settings: {...s.settings, ai: {...s.settings.ai, saveConversationHistory: false}},
    }))
    const oldest = useChatStore.getState().threads[0]!.id
    giveHistory(oldest, 1)
    useChatStore.getState().createThread('flowB')
    expect(useChatStore.getState().threads.some(t => t.id === oldest)).toBe(false)
    // The clear decides once its settings module has loaded.
    await vi.dynamicImportSettled()
    await new Promise(r => setTimeout(r, 0))
    expect(chatApi.clearConversation).not.toHaveBeenCalled()
  })
})
