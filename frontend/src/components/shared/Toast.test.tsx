import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import {ToastProvider, useToast} from './Toast'

function Probe() {
  const toast = useToast()
  return (
    <>
      <button
        onClick={() => toast.success('With undo', {action: {label: 'Undo', onClick: () => {}}})}
        data-testid="fire-action"
      >
        a
      </button>
      <button onClick={() => toast.success('Plain')} data-testid="fire-plain">
        p
      </button>
      <button onClick={() => toast.error('Bad')} data-testid="fire-error">
        e
      </button>
    </>
  )
}

// Duration policy (V1.2): action-bearing and error toasts persist 8s — the
// 4s default dismissed undo windows before users could reach them.
describe('Toast duration policy', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('action and error toasts outlast plain ones', () => {
    render(
      <ToastProvider>
        <Probe />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByTestId('fire-action'))
    fireEvent.click(screen.getByTestId('fire-plain'))
    fireEvent.click(screen.getByTestId('fire-error'))

    act(() => {
      vi.advanceTimersByTime(4500)
    })
    // Plain (4s) is gone; action + error (8s) remain.
    expect(screen.queryByText('Plain')).not.toBeInTheDocument()
    expect(screen.getByText('With undo')).toBeInTheDocument()
    expect(screen.getByText('Bad')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(4000)
    })
    expect(screen.queryByText('With undo')).not.toBeInTheDocument()
    expect(screen.queryByText('Bad')).not.toBeInTheDocument()
  })
})

// Regression: the manual-dismiss path scheduled `setTimeout(() => onClose(id), 200)`
// for the exit animation without keeping the handle, so nothing cancelled it.
// Unmounting the provider (route change, logout teardown) inside that window left
// a live timer holding a closure over a torn-down tree.
//
// Asserted via the pending-timer count rather than "does it throw": React 18
// dropped the setState-after-unmount warning, so a leaked timer is otherwise
// completely silent.
describe('Toast dismiss timer cleanup', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('cancels the deferred close when unmounted mid-animation', () => {
    const {unmount} = render(
      <ToastProvider>
        <Probe />
      </ToastProvider>,
    )
    act(() => {
      fireEvent.click(screen.getByTestId('fire-plain'))
    })
    act(() => {
      fireEvent.click(screen.getByLabelText('Dismiss'))
    })
    // In-flight: the 200ms exit timer is armed.
    expect(vi.getTimerCount()).toBeGreaterThan(0)

    unmount()

    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels the auto-dismiss timer on unmount', () => {
    const {unmount} = render(
      <ToastProvider>
        <Probe />
      </ToastProvider>,
    )
    act(() => {
      fireEvent.click(screen.getByTestId('fire-plain'))
    })
    expect(vi.getTimerCount()).toBeGreaterThan(0)

    unmount()

    expect(vi.getTimerCount()).toBe(0)
  })

  it('a second dismiss does not schedule a duplicate close', () => {
    render(
      <ToastProvider>
        <Probe />
      </ToastProvider>,
    )
    act(() => {
      fireEvent.click(screen.getByTestId('fire-plain'))
    })
    const dismiss = screen.getByLabelText('Dismiss')
    act(() => {
      fireEvent.click(dismiss)
    })
    const afterFirst = vi.getTimerCount()
    act(() => {
      fireEvent.click(dismiss)
    })
    expect(vi.getTimerCount()).toBe(afterFirst)
  })
})
