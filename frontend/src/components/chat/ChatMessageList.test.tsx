import type {ReactNode} from 'react'
import {render, screen} from '@testing-library/react'
import {describe, it, expect, vi} from 'vitest'
import ChatMessageList from './ChatMessageList'
import type {ChatMessage} from '@/types'

// react-virtuoso renders nothing in jsdom (zero-height scroller); stub it
// with a passthrough that renders every row AND the footer, mirroring the
// FindingsTab test convention. The tests assert on wiring (data →
// itemContent, footer rendering), not on windowing behavior.
//
// `context` is forwarded exactly as the real Virtuoso does: it is how the
// per-render footer reaches a components object that must keep a stable
// identity (rebuilding that object remounts the whole scroller).
vi.mock('react-virtuoso', () => ({
  Virtuoso: ({
    data = [],
    itemContent,
    components,
    context,
  }: {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => ReactNode
    components?: {Footer?: (props: {context?: unknown}) => ReactNode}
    context?: unknown
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={i}>{itemContent(i, item)}</div>
      ))}
      {components?.Footer ? <components.Footer context={context} /> : null}
    </div>
  ),
}))

const msg = (id: string, content: string): ChatMessage =>
  ({id, role: 'user', content, timestamp: '2024-01-01T00:00:00Z'}) as ChatMessage

function renderList(overrides: Partial<Parameters<typeof ChatMessageList>[0]> = {}) {
  return render(
    <ChatMessageList
      messages={[msg('m1', 'first question'), msg('m2', 'second question')]}
      renderMessage={(_i, m) => <div data-testid={`bubble-${m.id}`}>{m.content}</div>}
      footer={<div data-testid="streaming-bubble">streaming…</div>}
      {...overrides}
    />,
  )
}

describe('ChatMessageList', () => {
  it('renders each message through renderMessage', () => {
    renderList()
    expect(screen.getByTestId('bubble-m1')).toHaveTextContent('first question')
    expect(screen.getByTestId('bubble-m2')).toHaveTextContent('second question')
  })

  it('renders the footer (thinking/streaming bubbles)', () => {
    renderList()
    expect(screen.getByTestId('streaming-bubble')).toBeInTheDocument()
    // Footer follows the last message in DOM order.
    const last = screen.getByTestId('bubble-m2')
    const streaming = screen.getByTestId('streaming-bubble')
    expect(last.compareDocumentPosition(streaming) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('renders with no messages (welcome/state handled by the parent)', () => {
    renderList({messages: [], footer: undefined})
    expect(screen.queryByTestId('bubble-m1')).not.toBeInTheDocument()
  })
})
