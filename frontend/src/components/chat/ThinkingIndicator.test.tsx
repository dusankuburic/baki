import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import ThinkingIndicator from './ThinkingIndicator'

describe('ThinkingIndicator', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('announces the thinking state once, with the dots hidden from assistive tech', () => {
    render(<ThinkingIndicator />)
    expect(screen.getByRole('status').textContent).toBe('Thinking...')
    const dots = document.querySelectorAll('.thinking-dot')
    expect(dots).toHaveLength(3)
    expect(dots[0].parentElement?.getAttribute('aria-hidden')).toBe('true')
  })

  it('shows how long the model has been thinking once a few seconds have passed', () => {
    render(<ThinkingIndicator />)
    expect(screen.queryByText('2s')).toBeNull()
    act(() => {
      vi.advanceTimersByTime(2000)
    })
    expect(screen.queryByText('2s')).toBeNull()
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(screen.getByText('3s')).toBeTruthy()

    act(() => {
      vi.advanceTimersByTime(62_000)
    })
    const counter = screen.getByText('1m 05s')
    // Decorative for a screen reader: the status text itself never ticks.
    expect(counter.getAttribute('aria-hidden')).toBe('true')
    expect(screen.getByRole('status').textContent).toBe('Thinking...')
  })

  it('stops ticking when it unmounts', () => {
    const {unmount} = render(<ThinkingIndicator />)
    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})
