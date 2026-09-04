import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import CommandPalette from './CommandPalette'

const commands = [
  {id: 'a', label: 'Open file', section: 'File', onSelect: vi.fn()},
  {id: 'b', label: 'Save file', section: 'File', onSelect: vi.fn()},
  {id: 'c', label: 'Toggle sidebar', section: 'View', onSelect: vi.fn()},
]

// Regression: the rows carried role="option" with no role="listbox" owner — an
// invalid ARIA parent/child pairing that assistive tech discards outright — and
// the panel announced neither dialog nor modal. Because focus stays on the input
// while ArrowDown only moves a visual highlight, without aria-activedescendant a
// screen reader user got no indication of which command was selected.
describe('CommandPalette accessibility', () => {
  it('exposes the panel as a modal dialog', () => {
    render(<CommandPalette isOpen onClose={vi.fn()} commands={commands} />)
    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(dialog).toHaveAccessibleName('Command palette')
  })

  it('owns its options with a listbox and groups them by section', () => {
    render(<CommandPalette isOpen onClose={vi.fn()} commands={commands} />)
    const listbox = screen.getByRole('listbox')
    expect(listbox).toBeInTheDocument()
    // Every option must resolve to that listbox, not float unowned.
    const options = screen.getAllByRole('option')
    expect(options).toHaveLength(3)
    for (const opt of options) expect(listbox).toContainElement(opt)
    expect(screen.getAllByRole('group')).toHaveLength(2) // File, View
  })

  it('wires the input as a combobox tracking the active option', () => {
    render(<CommandPalette isOpen onClose={vi.fn()} commands={commands} />)
    const input = screen.getByRole('combobox')
    expect(input).toHaveAttribute('aria-controls', 'command-palette-listbox')

    fireEvent.keyDown(input, {key: 'ArrowDown'})
    const active = screen.getAllByRole('option').find(o => o.getAttribute('aria-selected') === 'true')
    expect(active).toBeTruthy()
    expect(input.getAttribute('aria-activedescendant')).toBe(active!.id)
    expect(active!.id).not.toBe('')
  })
})
