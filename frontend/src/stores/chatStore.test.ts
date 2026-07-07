import {describe, it, expect, beforeEach} from 'vitest'
import {useChatStore, MAX_CONCURRENT_STREAMS} from './chatStore'
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
        expect(slot).toEqual({streamId: 'stream1', messageId: 'msg1', text: '', isThinking: true, tokens: 0, toolStatus: null})
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
})
