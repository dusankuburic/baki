import {describe, it, expect, beforeEach} from 'vitest'
import {useChatStore} from './chatStore'
import type {ChatMessage} from '@/types/domain'

function makeMessage(id: string, role: 'user' | 'assistant', content = 'hello'): ChatMessage {
    return {id, role, content, timestamp: new Date().toISOString()} as ChatMessage
}

beforeEach(() => {
    // Reset store to initial state before each test
    useChatStore.setState({
        threads: [],
        activeThreadId: null,
        conversations: new Map(),
        activeStreamId: null,
        streamingMessageId: null,
        streamingText: '',
        selectedProvider: 'claude',
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

// ---- streaming ----

describe('streaming', () => {
    it('startStream sets activeStreamId and messageId', () => {
        useChatStore.getState().startStream('stream1', 'msg1')
        const state = useChatStore.getState()
        expect(state.activeStreamId).toBe('stream1')
        expect(state.streamingMessageId).toBe('msg1')
        expect(state.streamingText).toBe('')
    })

    it('updateStreamingMessage updates text', () => {
        useChatStore.getState().startStream('stream1', 'msg1')
        useChatStore.getState().updateStreamingMessage('Hello world')
        expect(useChatStore.getState().streamingText).toBe('Hello world')
    })

    it('endStream clears streaming state', () => {
        useChatStore.getState().startStream('stream1', 'msg1')
        useChatStore.getState().updateStreamingMessage('partial')
        useChatStore.getState().endStream()
        const state = useChatStore.getState()
        expect(state.activeStreamId).toBeNull()
        expect(state.streamingMessageId).toBeNull()
        expect(state.streamingText).toBe('')
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
