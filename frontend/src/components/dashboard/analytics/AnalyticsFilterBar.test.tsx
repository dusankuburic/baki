import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import {useOrgStore} from '@/stores/orgStore'
import {AnalyticsFilterBar} from './AnalyticsFilterBar'

const realSetPreset = useAnalyticsStore.getState().setPreset

beforeEach(() => {
  useAnalyticsStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null})
})

afterEach(() => {
  useAnalyticsStore.setState({setPreset: realSetPreset})
})

function renderBar() {
  return render(<AnalyticsFilterBar facets={null} applied={null} range={null} busy={false} />)
}

describe('AnalyticsFilterBar: the time range', () => {
  it('moves along the presets with the arrow keys without picking one, and picks on activation', () => {
    const setPreset = vi.fn()
    useAnalyticsStore.setState({setPreset})
    renderBar()
    const range = screen.getByRole('radiogroup', {name: 'Time range'})
    const selected = within(range).getByRole('radio', {name: '30 days'})
    expect(selected).toHaveAttribute('aria-checked', 'true')

    selected.focus()
    fireEvent.keyDown(selected, {key: 'ArrowRight'})
    const ninety = within(range).getByRole('radio', {name: '90 days'})
    expect(ninety).toHaveFocus()
    fireEvent.keyDown(ninety, {key: 'ArrowLeft'})
    fireEvent.keyDown(document.activeElement!, {key: 'ArrowLeft'})
    const seven = within(range).getByRole('radio', {name: '7 days'})
    expect(seven).toHaveFocus()
    expect(setPreset).not.toHaveBeenCalled()
    expect(selected).toHaveAttribute('aria-checked', 'true')

    fireEvent.click(seven)
    expect(setPreset).toHaveBeenCalledTimes(1)
    expect(setPreset).toHaveBeenCalledWith('7d')
  })

  it('applies a picked preset to the filters', () => {
    renderBar()
    fireEvent.click(screen.getByRole('radio', {name: '90 days'}))
    expect(useAnalyticsStore.getState().filters.preset).toBe('90d')
    expect(screen.getByRole('radio', {name: '90 days'})).toHaveAttribute('aria-checked', 'true')
  })
})
