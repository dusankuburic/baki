import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import SlashCommandAutocomplete from './SlashCommandAutocomplete'

// The menu listens for keys on window (capture phase). It used to act on keys
// typed ANYWHERE while it had matches: after typing "/c" in the composer, a Tab
// or Enter pressed in a search box ran /clear.

function setup(opts: {canClear?: boolean} = {}) {
  const composer = document.createElement('textarea')
  const elsewhere = document.createElement('input')
  document.body.append(composer, elsewhere)
  const onSelect = vi.fn()
  const onClose = vi.fn()
  render(
    <SlashCommandAutocomplete
      query="c"
      onSelect={onSelect}
      onClose={onClose}
      isOwnTarget={t => t === composer}
      canClear={opts.canClear}
    />,
  )
  return {composer, elsewhere, onSelect, onClose}
}

describe('SlashCommandAutocomplete', () => {
  it('selects with Enter typed in its composer', () => {
    const {composer, onSelect} = setup()
    fireEvent.keyDown(composer, {key: 'Enter'})
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({id: '/clear'}))
  })

  it('ignores Enter, Tab and Escape typed anywhere else in the app', () => {
    const {elsewhere, onSelect, onClose} = setup()
    fireEvent.keyDown(elsewhere, {key: 'Enter'})
    fireEvent.keyDown(elsewhere, {key: 'Tab'})
    fireEvent.keyDown(elsewhere, {key: 'Escape'})
    expect(onSelect).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()
  })

  it('does not offer /clear where the thread cannot be cleared', () => {
    setup({canClear: false})
    expect(screen.queryByText('Clear')).not.toBeInTheDocument()
  })
})
