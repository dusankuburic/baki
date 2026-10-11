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
