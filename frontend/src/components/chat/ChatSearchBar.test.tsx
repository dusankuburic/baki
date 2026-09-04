import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatSearchBar from './ChatSearchBar'

function renderBar(overrides: Partial<Parameters<typeof ChatSearchBar>[0]> = {}) {
  const props = {
    query: 'timeout',
    onChange: vi.fn(),
    current: 2,
    total: 5,
    onPrev: vi.fn(),
    onNext: vi.fn(),
    onClose: vi.fn(),
    ...overrides,
  }
  render(<ChatSearchBar {...props} />)
  return props
}

describe('ChatSearchBar', () => {
  it('reports the position within the match list, not just a count', () => {
    renderBar()
    expect(screen.getByText('2 of 5')).toBeInTheDocument()
  })

  it('walks matches with Enter and Shift+Enter', async () => {
    const user = userEvent.setup()
    const props = renderBar()
    const input = screen.getByRole('textbox')

    await user.type(input, '{Enter}')
    expect(props.onNext).toHaveBeenCalledTimes(1)

    await user.type(input, '{Shift>}{Enter}{/Shift}')
    expect(props.onPrev).toHaveBeenCalledTimes(1)
  })

  it('disables stepping and says so when nothing matches', () => {
    renderBar({total: 0, current: 0})
    expect(screen.getByText('No matches')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Next match'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Previous match'})).toBeDisabled()
  })

  it('closes on Escape', async () => {
    const user = userEvent.setup()
    const props = renderBar()
    await user.type(screen.getByRole('textbox'), '{Escape}')
    expect(props.onClose).toHaveBeenCalled()
  })
})
