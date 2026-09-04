import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import MessageBubble from './MessageBubble'
import type {ChatMessage} from '@/types'

function msg(role: ChatMessage['role'], content: string): ChatMessage {
  return {id: 'm1', role, content, timestamp: '2024-01-01T00:00:00Z'}
}

// Search marks occurrences in place rather than filtering the conversation, so
// the highlight has to survive BOTH render paths: plain user text and the
// markdown pipeline used for assistant answers.
describe('MessageBubble search highlighting', () => {
  it('marks matches in a user message', () => {
    render(<MessageBubble message={msg('user', 'the Timeout is zero')} highlight="timeout" />)
    const marks = document.querySelectorAll('mark.chat-search-hit')
    expect(marks).toHaveLength(1)
    // Case-insensitive match keeps the original casing on screen.
    expect(marks[0].textContent).toBe('Timeout')
  })

  it('marks matches inside rendered markdown', () => {
    render(<MessageBubble message={msg('assistant', 'Set the **timeout** and the timeout again.')} highlight="timeout" />)
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(2)
  })

  it('renders nothing marked without a query', () => {
    render(<MessageBubble message={msg('assistant', 'plain timeout text')} />)
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(0)
  })

  it('treats the query as literal text, not a pattern', () => {
    // A bare ( would throw when spliced into a RegExp unescaped.
    render(<MessageBubble message={msg('user', 'call foo(bar) now')} highlight="foo(bar)" />)
    expect(document.querySelector('mark.chat-search-hit')?.textContent).toBe('foo(bar)')
  })

  // Regression: `components.text` is keyed by ELEMENT name, so react-markdown
  // silently ignored it — @-mentions in assistant answers rendered as plain
  // text for as long as that override existed. The transformation happens in a
  // rehype pass now, so it actually reaches the tree.
  it('turns @-mentions in an ASSISTANT (markdown) message into jump buttons', () => {
    render(<MessageBubble message={msg('assistant', 'see @Login.txt for details')} />)
    expect(screen.getByRole('button', {name: 'Go to Login.txt'})).toBeInTheDocument()
  })

  it('leaves mentions and matches inside code spans alone', () => {
    render(<MessageBubble message={msg('assistant', 'use `@Login.txt` and `timeout`')} highlight="timeout" />)
    // Code is quoted verbatim: a pill or a mark there would misreport the text.
    expect(screen.queryByRole('button', {name: /Go to/})).not.toBeInTheDocument()
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(0)
  })

  it('keeps @-mentions clickable while a search is active', () => {
    render(<MessageBubble message={msg('user', 'see @Login.txt for the timeout')} highlight="timeout" />)
    expect(screen.getByRole('button', {name: 'Go to Login.txt'})).toBeInTheDocument()
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(1)
  })
})
