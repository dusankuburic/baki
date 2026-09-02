import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Tabs from './Tabs'
import {FolderTree, BarChart2} from 'lucide-react'

const items = [
  {value: 'explorer' as const, label: 'Explorer', icon: FolderTree},
  {value: 'variables' as const, label: 'Variables', icon: BarChart2},
  {value: 'library' as const, label: 'Library'},
]

// The accessible tab strip contract (U5a.1): tablist/tab roles, roving
// tabindex, arrow/Home/End navigation with selection-follows-focus.
describe('Tabs', () => {
  it('renders tablist semantics with aria-selected and roving tabindex', () => {
    const onChange = vi.fn()
    render(<Tabs items={items} value="explorer" onChange={onChange} aria-label="Sections" />)
    const list = screen.getByRole('tablist', {name: 'Sections'})
    const tabs = screen.getAllByRole('tab')
    expect(tabs).toHaveLength(3)
    expect(tabs[0]).toHaveAttribute('aria-selected', 'true')
    expect(tabs[1]).toHaveAttribute('aria-selected', 'false')
    expect(tabs[0]).toHaveAttribute('tabindex', '0')
    expect(tabs[1]).toHaveAttribute('tabindex', '-1')
    expect(list).toBeTruthy()
  })

  it('arrow keys move selection (selection follows focus)', () => {
    const onChange = vi.fn()
    render(<Tabs items={items} value="explorer" onChange={onChange} aria-label="Sections" />)
    fireEvent.keyDown(screen.getByRole('tablist'), {key: 'ArrowRight'})
    expect(onChange).toHaveBeenCalledWith('variables')
    fireEvent.keyDown(screen.getByRole('tablist'), {key: 'ArrowLeft'})
    expect(onChange).toHaveBeenCalledWith('library') // wraps from first to last
    fireEvent.keyDown(screen.getByRole('tablist'), {key: 'Home'})
    expect(onChange).toHaveBeenLastCalledWith('explorer')
    fireEvent.keyDown(screen.getByRole('tablist'), {key: 'End'})
    expect(onChange).toHaveBeenLastCalledWith('library')
  })

  it('wires aria-controls when a panel prefix is given', () => {
    render(<Tabs items={items} value="explorer" onChange={vi.fn()} aria-label="Sections" panelIdPrefix="p" />)
    expect(screen.getByRole('tab', {name: /explorer/i})).toHaveAttribute('aria-controls', 'p-explorer')
  })
})
