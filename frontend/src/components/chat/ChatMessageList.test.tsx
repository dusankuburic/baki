import type {ReactNode} from 'react'
import {render, screen, fireEvent} from '@testing-library/react'
import {describe, it, expect, vi} from 'vitest'
import ChatMessageList from './ChatMessageList'
import type {ChatMessage} from '@/types'

const scrollToIndexSpy = vi.hoisted(() => vi.fn())
const scrollToSpy = vi.hoisted(() => vi.fn())
vi.mock('react-virtuoso', async () => {
  const React = await import('react')
  interface MockProps {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => ReactNode
    components?: {Footer?: (props: {context?: unknown}) => ReactNode}
    context?: unknown
    scrollerRef?: (el: HTMLElement | null) => void
  }
  const Virtuoso = React.forwardRef<unknown, MockProps>(function Virtuoso(
    {data = [], itemContent, components, context, scrollerRef},
    ref,
  ) {
    const scrollerEl = React.useRef<HTMLDivElement | null>(null)
    React.useImperativeHandle(ref, () => ({scrollToIndex: scrollToIndexSpy}))
    React.useEffect(() => {
      const el = scrollerEl.current
      if (!el) return
      Object.defineProperty(el, 'scrollHeight', {value: 5000, configurable: true})
      Object.defineProperty(el, 'clientHeight', {value: 600, configurable: true})
      Object.defineProperty(el, 'scrollTop', {value: 0, writable: true, configurable: true})
      el.scrollTo = scrollToSpy as unknown as typeof el.scrollTo
      scrollerRef?.(el)
      return () => scrollerRef?.(null)
    }, [scrollerRef])
    return (
      <div ref={scrollerEl} data-testid="scroller">
        {data.map((item, i) => (
          <div key={i}>{itemContent(i, item)}</div>
        ))}
        {components?.Footer ? <components.Footer context={context} /> : null}
      </div>
    )
  })
  return {Virtuoso}
})

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
    const last = screen.getByTestId('bubble-m2')
    const streaming = screen.getByTestId('streaming-bubble')
    expect(last.compareDocumentPosition(streaming) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('renders with no messages (welcome/state handled by the parent)', () => {
    renderList({messages: [], footer: undefined})
    expect(screen.queryByTestId('bubble-m1')).not.toBeInTheDocument()
  })
})

describe('ChatMessageList scroll-to-bottom', () => {
  it('scrolls the scroller itself to its full height, not a Virtuoso seek', () => {
    scrollToSpy.mockClear()
    scrollToIndexSpy.mockClear()
    renderList()
    fireEvent.click(screen.getByRole('button', {name: 'Scroll to bottom'}))
    expect(scrollToSpy).toHaveBeenCalledWith({top: 5000, behavior: 'smooth'})
    expect(scrollToIndexSpy).not.toHaveBeenCalled()
  })

  it('hides the button once the scroller reports it is at the bottom', () => {
    renderList()
    const scroller = screen.getByTestId('scroller')
    expect(screen.getByRole('button', {name: 'Scroll to bottom'})).toBeInTheDocument()
    ;(scroller as HTMLElement).scrollTop = 4400
    fireEvent.scroll(scroller)
    expect(screen.queryByRole('button', {name: 'Scroll to bottom'})).not.toBeInTheDocument()
    ;(scroller as HTMLElement).scrollTop = 1000
    fireEvent.scroll(scroller)
    expect(screen.getByRole('button', {name: 'Scroll to bottom'})).toBeInTheDocument()
  })
})
