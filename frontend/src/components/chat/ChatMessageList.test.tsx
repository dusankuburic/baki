import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import type {ComponentType, ReactNode, Ref} from 'react'
import type {ChatMessage} from '@/types'

// A Virtuoso that mounts only the items from `firstMounted` on, through the list's own Scroller: scrolling up mounts
// earlier items, as the real one does with its viewport + 400 px window.
let firstMounted = 0
vi.mock('react-virtuoso', async () => {
  const {forwardRef} = await import('react')
  const Virtuoso = forwardRef(function Virtuoso(
    {
      data = [],
      itemContent,
      components,
      context,
    }: {
      data?: ChatMessage[]
      itemContent: (i: number, item: ChatMessage) => ReactNode
      components: {Scroller: ComponentType<{context?: unknown; children?: ReactNode; ref?: Ref<HTMLDivElement>}>}
      context?: unknown
    },
    _ref,
  ) {
    const Scroller = components.Scroller
    return (
      <Scroller context={context}>
        {data.slice(firstMounted).map((m, i) => (
          <div key={m.id}>{itemContent(firstMounted + i, m)}</div>
        ))}
      </Scroller>
    )
  })
  return {Virtuoso}
})

import ChatMessageList from './ChatMessageList'

const messages: ChatMessage[] = Array.from({length: 12}, (_, i) => ({
  id: `m${i}`,
  role: i % 2 === 0 ? ('user' as const) : ('assistant' as const),
  content: `message number ${i}`,
  timestamp: 't',
}))

const renderMessage = (_i: number, m: ChatMessage) => <p>{m.content}</p>

/** Text of every element assistive tech treats as a live region: aria-live other than off, or an implicitly live role. */
function liveRegionTexts(root: HTMLElement): string[] {
  const live = Array.from(
    root.querySelectorAll<HTMLElement>('[aria-live], [role="status"], [role="log"], [role="alert"]'),
  )
  return live.filter(el => el.getAttribute('aria-live') !== 'off').map(el => el.textContent ?? '')
}

// R-frontend-5: the virtualised scroller was a polite live region, so scrolling up (Virtuoso mounting earlier items)
// re-announced old messages as additions.
describe('ChatMessageList live region', () => {
  it('keeps role="log" but is not a live region', () => {
    firstMounted = 8
    render(<ChatMessageList messages={messages} renderMessage={renderMessage} />)
    const log = screen.getByRole('log')
    expect(log).not.toHaveAttribute('aria-relevant')
    // role=log is implicitly aria-live="polite"; only an explicit "off" stops a scroll from being read as additions.
    expect(log).toHaveAttribute('aria-live', 'off')
  })

  it('changing the viewport (mounting earlier items) changes no live-region text', () => {
    firstMounted = 8
    const {container, rerender} = render(<ChatMessageList messages={messages} renderMessage={renderMessage} />)
    const before = liveRegionTexts(container)

    firstMounted = 2
    rerender(<ChatMessageList messages={[...messages]} renderMessage={renderMessage} />)
    expect(screen.getByText('message number 2')).toBeInTheDocument()
    expect(liveRegionTexts(container)).toEqual(before)
  })
})
