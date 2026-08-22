import {describe, it, expect} from 'vitest'
import {parseChatEvent, chatEventStreamId} from './chatEvent'

describe('parseChatEvent', () => {
  it('parses a chunk envelope with content', () => {
    expect(parseChatEvent({streamId: 's1', type: 'chunk', data: {content: 'hel'}})).toEqual({
      kind: 'chunk',
      content: 'hel',
    })
  })

  it('defaults a missing chunk content to empty string (RAF tail tolerates it)', () => {
    expect(parseChatEvent({streamId: 's1', type: 'chunk', data: {}})).toEqual({kind: 'chunk', content: ''})
  })

  it('parses a done envelope with token counts and chunk total', () => {
    expect(parseChatEvent({streamId: 's1', type: 'done', data: {tokensOut: 42, tokensIn: 10, chunks: 7}})).toEqual({
      kind: 'done',
      tokensOut: 42,
      tokensIn: 10,
      chunks: 7,
    })
  })

  it('tolerates missing counts on done', () => {
    expect(parseChatEvent({streamId: 's1', type: 'done'})).toEqual({
      kind: 'done',
      tokensOut: 0,
      tokensIn: 0,
      chunks: undefined,
    })
  })

  it('parses an error envelope, defaulting the message', () => {
    expect(parseChatEvent({streamId: 's1', type: 'error', data: {message: 'boom'}})).toEqual({
      kind: 'error',
      message: 'boom',
    })
    expect(parseChatEvent({streamId: 's1', type: 'error'})).toEqual({kind: 'error', message: 'Unknown error'})
  })

  it('parses a tool envelope preferring label over name', () => {
    expect(parseChatEvent({streamId: 's1', type: 'tool', data: {label: 'Reading file', name: 'read'}})).toEqual({
      kind: 'tool',
      label: 'Reading file',
    })
    expect(parseChatEvent({streamId: 's1', type: 'tool', data: {name: 'read'}})).toEqual({
      kind: 'tool',
      label: 'read',
    })
    expect(parseChatEvent({streamId: 's1', type: 'tool'})).toEqual({kind: 'tool', label: 'Using tool'})
  })

  it('drops malformed payloads instead of casting them into the store', () => {
    // A proxy error page / different event's shape / truncated JSON.
    expect(parseChatEvent(null)).toBeNull()
    expect(parseChatEvent('<html>err</html>')).toBeNull()
    expect(parseChatEvent([])).toBeNull()
    expect(parseChatEvent({noStreamId: true, type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: '', type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: 's1', type: 'surprise'})).toBeNull()
    expect(parseChatEvent({streamId: 's1'})).toBeNull()
    // Non-finite numbers must not leak into token accounting.
    expect(parseChatEvent({streamId: 's1', type: 'done', data: {tokensOut: 'lots'}})).toEqual({
      kind: 'done',
      tokensOut: 0,
      tokensIn: 0,
      chunks: undefined,
    })
  })
})

describe('chatEventStreamId', () => {
  it('extracts the addressing streamId', () => {
    expect(chatEventStreamId({streamId: 's1', type: 'chunk'})).toBe('s1')
  })

  it('returns null for malformed envelopes', () => {
    expect(chatEventStreamId(undefined)).toBeNull()
    expect(chatEventStreamId({streamId: 42})).toBeNull()
    expect(chatEventStreamId('s1')).toBeNull()
  })
})
