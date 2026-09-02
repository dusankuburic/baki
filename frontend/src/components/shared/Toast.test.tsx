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
