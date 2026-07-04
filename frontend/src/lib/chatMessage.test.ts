import {describe, it, expect} from 'vitest'
import {parseChatMessage, parseChatMessages} from './chatMessage'
import type {ChatMessage} from '@/types'

describe('parseChatMessage', () => {
    it('parses a well-formed message', () => {
        const m = parseChatMessage({id: 'm1', role: 'user', content: 'hi', timestamp: '2024-01-01T00:00:00Z'})
        expect(m).toEqual({id: 'm1', role: 'user', content: 'hi', timestamp: '2024-01-01T00:00:00Z'})
    })

    it('preserves valid optional fields', () => {
        const m = parseChatMessage({
            id: 'm1', role: 'assistant', content: 'hello', timestamp: 't',
            contextBlockId: 'b1', tokensIn: 10, tokensOut: 5, model: 'gpt-x', provider: 'openai',
        })
        expect(m?.contextBlockId).toBe('b1')
        expect(m?.tokensIn).toBe(10)
        expect(m?.model).toBe('gpt-x')
        expect(m?.provider).toBe('openai')
    })

    it('rejects payloads missing id or timestamp', () => {
        expect(parseChatMessage({role: 'user', content: 'x', timestamp: 't'})).toBeNull()
        expect(parseChatMessage({id: 'm1', role: 'user', content: 'x'})).toBeNull()
    })

    it('rejects non-object payloads', () => {
        expect(parseChatMessage(null)).toBeNull()
        expect(parseChatMessage('hello')).toBeNull()
        expect(parseChatMessage(undefined)).toBeNull()
    })

    it('coerces an unknown role to assistant and defaults non-string content', () => {
        const m = parseChatMessage({id: 'm1', role: 'wizard', content: 42, timestamp: 't'})
        expect(m?.role).toBe('assistant')
        expect(m?.content).toBe('')
    })

    it('drops optional fields of the wrong type', () => {
        const m = parseChatMessage({id: 'm1', role: 'user', content: 'x', timestamp: 't', tokensIn: 'lots', model: 123})
        expect(m).toEqual({id: 'm1', role: 'user', content: 'x', timestamp: 't'})
    })
})

describe('parseChatMessages', () => {
    it('drops invalid entries and keeps valid ones in order', () => {
        const valid: ChatMessage = {id: 'ok', role: 'user', content: 'a', timestamp: 't'}
        const result = parseChatMessages([
            valid,
            {role: 'user', content: 'no id', timestamp: 't'}, // dropped
            {id: 'ok2', role: 'assistant', content: 'b', timestamp: 't2'},
        ])
        expect(result).toHaveLength(2)
        expect(result[0].id).toBe('ok')
        expect(result[1].id).toBe('ok2')
    })

    it('returns an empty list for non-array payloads', () => {
        expect(parseChatMessages(undefined)).toEqual([])
        expect(parseChatMessages({messages: []})).toEqual([])
        expect(parseChatMessages(null)).toEqual([])
    })
})
