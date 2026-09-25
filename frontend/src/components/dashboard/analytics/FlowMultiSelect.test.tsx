import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import {FlowMultiSelect} from './FlowMultiSelect'

const flows = [
  {id: 'f1', name: 'Invoice sync', health: 42, findings: 14, analyzedAt: '2026-09-20T10:00:00Z'},
  {id: 'f2', name: 'Customer onboarding', health: 61, findings: 9, analyzedAt: '2026-09-21T10:00:00Z'},
  {id: 'f3', name: 'Nightly export', health: null, findings: 0, analyzedAt: null},
]

describe('FlowMultiSelect', () => {
  it('opens a listbox with health and finding counts and toggles a flow', () => {
    const onChange = vi.fn()
    render(<FlowMultiSelect flows={flows} selected={[]} truncated={false} onChange={onChange} />)
    const trigger = screen.getByRole('button', {name: 'Flows: All flows'})
    expect(trigger).toHaveTextContent('All flows')
    expect(trigger).toHaveAttribute('aria-expanded', 'false')

    fireEvent.click(trigger)
    const list = screen.getByRole('listbox', {name: 'Flows in scope'})
    expect(within(list).getAllByRole('option')).toHaveLength(3)
    expect(within(list).getByRole('option', {name: /Invoice sync/})).toHaveTextContent('42')
    expect(within(list).getByRole('option', {name: /Nightly export/})).toHaveTextContent('—')

    fireEvent.click(within(list).getByRole('option', {name: /Customer onboarding/}))
    expect(onChange).toHaveBeenCalledWith(['f2'])
  })

  it('filters by search and selects the visible flows', () => {
    const onChange = vi.fn()
    render(<FlowMultiSelect flows={flows} selected={['f1']} truncated={false} onChange={onChange} />)
    expect(screen.getByRole('button', {name: 'Flows: 1 flow'})).toHaveTextContent('1 flow')
    fireEvent.click(screen.getByRole('button', {name: 'Flows: 1 flow'}))
    fireEvent.change(screen.getByRole('searchbox', {name: 'Search flows'}), {target: {value: 'night'}})
    const list = screen.getByRole('listbox', {name: 'Flows in scope'})
    expect(within(list).getAllByRole('option')).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', {name: 'Select visible'}))
    expect(onChange).toHaveBeenCalledWith(['f1', 'f3'])
    fireEvent.click(screen.getByRole('button', {name: 'Clear'}))
    expect(onChange).toHaveBeenCalledWith([])
  })

  it('navigates with the keyboard and returns focus to the button on Escape', () => {
    const onChange = vi.fn()
    render(<FlowMultiSelect flows={flows} selected={[]} truncated={true} onChange={onChange} />)
    const trigger = screen.getByRole('button', {name: 'Flows: All flows'})
    fireEvent.click(trigger)
    const search = screen.getByRole('searchbox', {name: 'Search flows'})
    fireEvent.keyDown(search, {key: 'ArrowDown'})
    fireEvent.keyDown(search, {key: 'Enter'})
    expect(onChange).toHaveBeenCalledWith(['f2'])
    expect(screen.getByText(/Showing the first 3 flows/)).toBeInTheDocument()
    fireEvent.keyDown(search, {key: 'Escape'})
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(trigger).toHaveFocus()
  })
})
