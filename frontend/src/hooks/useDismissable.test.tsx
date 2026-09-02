import {describe, it, expect, vi} from 'vitest'
import {render, fireEvent, screen} from '@testing-library/react'
import {useDismissable} from './useDismissable'

function Probe({active, onDismiss}: {active: boolean; onDismiss: () => void}) {
  const ref = useDismissable(active, onDismiss)
  return (
    <div>
      <div ref={ref} data-testid="popover">
        <button>inside</button>
      </div>
      <button data-testid="outside">outside</button>
    </div>
  )
}

// The shared popover dismissal contract (U1.5): outside mousedown and Escape
// close; clicks inside never do.
describe('useDismissable', () => {
  it('dismisses on outside mousedown while active', () => {
    const onDismiss = vi.fn()
    render(<Probe active onDismiss={onDismiss} />)
    fireEvent.click(screen.getByTestId('outside'))
    expect(onDismiss).not.toHaveBeenCalled() // click isn't mousedown
    fireEvent.mouseDown(screen.getByTestId('outside'))
    expect(onDismiss).toHaveBeenCalledTimes(1)
  })

  it('ignores mousedown inside the popover', () => {
    const onDismiss = vi.fn()
    render(<Probe active onDismiss={onDismiss} />)
    fireEvent.mouseDown(screen.getByText('inside'))
    expect(onDismiss).not.toHaveBeenCalled()
  })

  it('dismisses on Escape while active', () => {
    const onDismiss = vi.fn()
    render(<Probe active onDismiss={onDismiss} />)
    fireEvent.keyDown(document, {key: 'Escape'})
    expect(onDismiss).toHaveBeenCalledTimes(1)
  })

  it('does nothing when inactive (listeners removed)', () => {
    const onDismiss = vi.fn()
    render(<Probe active={false} onDismiss={onDismiss} />)
    fireEvent.mouseDown(screen.getByTestId('outside'))
    fireEvent.keyDown(document, {key: 'Escape'})
    expect(onDismiss).not.toHaveBeenCalled()
  })

  it('re-arms when active flips (state-driven dismissal loop)', () => {
    const onDismiss = vi.fn()
    const {rerender} = render(<Probe active={false} onDismiss={onDismiss} />)
    rerender(<Probe active onDismiss={onDismiss} />)
    fireEvent.keyDown(document, {key: 'Escape'})
    expect(onDismiss).toHaveBeenCalledTimes(1)
  })

  it("invokes the latest render's callback without re-binding listeners", () => {
    const calls: string[] = []
    const {rerender} = render(<Tagged tag="first" onCall={calls.push.bind(calls)} />)
    rerender(<Tagged tag="second" onCall={calls.push.bind(calls)} />)
    fireEvent.keyDown(document, {key: 'Escape'})
    expect(calls).toEqual(['second'])
  })
})

function Tagged({tag, onCall}: {tag: string; onCall: (t: string) => void}) {
  useDismissable(true, () => onCall(tag))
  return null
}
