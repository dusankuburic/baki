import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import MessageBubble from './MessageBubble'
import type {ChatMessage} from '@/types'

function msg(role: ChatMessage['role'], content: string): ChatMessage {
  return {id: 'm1', role, content, timestamp: '2024-01-01T00:00:00Z'}
}

describe('MessageBubble search highlighting', () => {
  it('marks matches in a user message', () => {
    render(<MessageBubble message={msg('user', 'the Timeout is zero')} highlight="timeout" />)
    const marks = document.querySelectorAll('mark.chat-search-hit')
    expect(marks).toHaveLength(1)
    expect(marks[0]!.textContent).toBe('Timeout')
  })

  it('marks matches inside rendered markdown', () => {
    render(
      <MessageBubble message={msg('assistant', 'Set the **timeout** and the timeout again.')} highlight="timeout" />,
    )
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(2)
  })

  it('renders nothing marked without a query', () => {
    render(<MessageBubble message={msg('assistant', 'plain timeout text')} />)
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(0)
  })

  it('treats the query as literal text, not a pattern', () => {
    render(<MessageBubble message={msg('user', 'call foo(bar) now')} highlight="foo(bar)" />)
    expect(document.querySelector('mark.chat-search-hit')?.textContent).toBe('foo(bar)')
  })

  it('turns @-mentions in an ASSISTANT (markdown) message into jump buttons', () => {
    render(<MessageBubble message={msg('assistant', 'see @Login.txt for details')} />)
    expect(screen.getByRole('button', {name: 'Go to Login.txt'})).toBeInTheDocument()
  })

  it('leaves mentions and matches inside code spans alone', () => {
    render(<MessageBubble message={msg('assistant', 'use `@Login.txt` and `timeout`')} highlight="timeout" />)
    expect(screen.queryByRole('button', {name: /Go to/})).not.toBeInTheDocument()
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(0)
  })

  it('keeps @-mentions clickable while a search is active', () => {
    render(<MessageBubble message={msg('user', 'see @Login.txt for the timeout')} highlight="timeout" />)
    expect(screen.getByRole('button', {name: 'Go to Login.txt'})).toBeInTheDocument()
    expect(document.querySelectorAll('mark.chat-search-hit')).toHaveLength(1)
  })
})
