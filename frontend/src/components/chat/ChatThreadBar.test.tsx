import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, within, fireEvent} from '@testing-library/react'
import ChatThreadBar from './ChatThreadBar'
import {useChatStore} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'

const initialChatState = useChatStore.getState()

function makeThread(id: string, title = 'Thread'): ChatThread {
  return {
    id,
    flowId: 'flow1',
    title,
    createdAt: new Date().toISOString(),
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
  }
}

beforeEach(() => {
  useChatStore.setState(initialChatState, true)
})

describe('ChatThreadBar streaming indicator', () => {
  it('renders nothing when there are no threads', () => {
    const {container} = render(
      <ChatThreadBar
        threads={[]}
        activeThreadId={null}
        onSelect={() => {}}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('shows a generating indicator only on streaming threads', () => {
    const t1 = makeThread('t1', 'Idle thread')
    const t2 = makeThread('t2', 'Busy thread')
    useChatStore.getState().startStream('t2', 's2', 'm2')

    render(
      <ChatThreadBar
        threads={[t1, t2]}
        activeThreadId="t1"
        onSelect={() => {}}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )

    // The busy thread shows the "Generating…" indicator dot.
    const busyTab = screen.getByText('Busy thread').closest('[class*="group"]')!
    expect(within(busyTab as HTMLElement).getByTitle('Generating…')).toBeInTheDocument()

    // The idle thread does not.
    const idleTab = screen.getByText('Idle thread').closest('[class*="group"]')!
    expect(within(idleTab as HTMLElement).queryByTitle('Generating…')).not.toBeInTheDocument()
  })

  it('clears the indicator when the stream ends', () => {
    const t1 = makeThread('t1', 'Thread')
    useChatStore.getState().startStream('t1', 's1', 'm1')

    const {rerender} = render(
      <ChatThreadBar
        threads={[t1]}
        activeThreadId="t1"
        onSelect={() => {}}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )
    expect(screen.getByTitle('Generating…')).toBeInTheDocument()

    useChatStore.getState().endStream('t1')
    rerender(
      <ChatThreadBar
        threads={[t1]}
        activeThreadId="t1"
        onSelect={() => {}}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )
    expect(screen.queryByTitle('Generating…')).not.toBeInTheDocument()
  })
})

describe('ChatThreadBar accessibility', () => {
  it('exposes a tablist with role=tab and aria-selected on the active thread', () => {
    const t1 = makeThread('t1', 'First')
    const t2 = makeThread('t2', 'Second')
    render(
      <ChatThreadBar
        threads={[t1, t2]}
        activeThreadId="t2"
        onSelect={() => {}}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )
    const tabs = screen.getAllByRole('tab')
    expect(tabs).toHaveLength(2)
    expect(tabs[0]).toHaveAttribute('aria-selected', 'false')
    expect(tabs[1]).toHaveAttribute('aria-selected', 'true')
    // Only the active tab is in the tab order.
    expect(tabs[1]).toHaveAttribute('tabIndex', '0')
    expect(tabs[0]).toHaveAttribute('tabIndex', '-1')
  })

  it('ArrowRight selects the next thread (wraps at the end)', () => {
    const onSelect = vi.fn()
    const t1 = makeThread('t1', 'First')
    const t2 = makeThread('t2', 'Second')
    render(
      <ChatThreadBar
        threads={[t1, t2]}
        activeThreadId="t2"
        onSelect={onSelect}
        onCreate={() => {}}
        onClose={() => {}}
        onRename={() => {}}
      />,
    )
    const tabs = screen.getAllByRole('tab')
    fireEvent.keyDown(tabs[1], {key: 'ArrowRight'})
    expect(onSelect).toHaveBeenCalledWith('t1')
  })
})
