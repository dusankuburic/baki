import {describe, it, expect} from 'vitest'
import {conversationToMarkdown, safeFilename} from './chatExport'
import type {ChatMessage} from '@/types'

function msg(p: Partial<ChatMessage>): ChatMessage {
  return {id: 'm', role: 'user', content: '', timestamp: '2026-01-01T00:00:00Z', ...p} as ChatMessage
}

describe('conversationToMarkdown', () => {
  it('renders a titled transcript with role headers', () => {
    const md = conversationToMarkdown('My chat', [
      msg({role: 'user', content: 'hello'}),
      msg({role: 'assistant', content: 'hi there', model: 'claude-x'}),
    ])
    expect(md).toContain('# My chat')
    expect(md).toContain('## You')
    expect(md).toContain('## AI (claude-x)')
    expect(md).toContain('hello')
    expect(md).toContain('hi there')
  })

  it('drops error turns from the transcript', () => {
    const md = conversationToMarkdown('t', [
      msg({role: 'assistant', content: 'good answer'}),
      msg({role: 'assistant', content: '*Error: boom*', finishReason: 'error'}),
    ])
    expect(md).toContain('good answer')
    expect(md).not.toContain('boom')
  })

  it('keeps interrupted turns', () => {
    const md = conversationToMarkdown('t', [msg({role: 'assistant', content: 'partial', finishReason: 'interrupted'})])
    expect(md).toContain('partial')
  })
})

describe('safeFilename', () => {
  it('sanitizes a title into a chat-*.md basename', () => {
    expect(safeFilename('Fix: Hardcoded Password!')).toBe('chat-fix-hardcoded-password.md')
  })

  it('falls back when the title has no usable characters', () => {
    expect(safeFilename('!!!')).toBe('chat-conversation.md')
    expect(safeFilename('')).toBe('chat-conversation.md')
  })
})

describe('safeFilename (non-ASCII titles)', () => {
  it('keeps letters of any script instead of dropping them', () => {
    expect(safeFilename('Šta radi ovaj flow')).toBe('chat-šta-radi-ovaj-flow.md')
    expect(safeFilename('日本語のフロー')).toBe('chat-日本語のフロー.md')
  })

  it('still strips path separators and reserved characters', () => {
    expect(safeFilename('../etc/passwd: "x"')).toBe('chat-etc-passwd-x.md')
  })
})

describe('safeFilename (combining marks)', () => {
  it('keeps vowel signs and decomposed accents as part of their letters', () => {
    expect(safeFilename('नमस्ते')).toBe('chat-नमस्ते.md')
    // "S" + combining caron: normalised to "š", not split at the mark.
    expect(safeFilename('S\u030Cta')).toBe('chat-šta.md')
  })
})
