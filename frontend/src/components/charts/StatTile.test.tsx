import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {StatTile} from './StatTile'

describe('StatTile', () => {
  it('renders label, value and sub-line as one accessible group', () => {
    render(<StatTile label="Flows" value="16" sub="179 subflows" />)
    expect(screen.getByRole('group', {name: 'Flows, 16, 179 subflows'})).toBeInTheDocument()
    expect(screen.getByText('16')).toBeInTheDocument()
  })

  it('colours the delta by direction × whether up is good', () => {
    const {rerender} = render(
      <StatTile
        label="Findings"
        value="120"
        delta={{current: 120, prev: 100, goodWhen: 'down', periodLabel: 'vs last 30 days'}}
      />,
    )
    const up = screen.getByText('up +20% vs last 30 days')
    expect(up).toHaveClass('text-semantic-error')

    rerender(
      <StatTile
        label="Health"
        value="84"
        delta={{current: 84, prev: 70, goodWhen: 'up', periodLabel: 'vs last 30 days'}}
      />,
    )
    expect(screen.getByText('up +20% vs last 30 days')).toHaveClass('text-semantic-success')

    rerender(
      <StatTile
        label="Spend"
        value="$2"
        delta={{current: 2, prev: 2, goodWhen: 'neutral', periodLabel: 'vs last 30 days'}}
      />,
    )
    expect(screen.getByText('unchanged vs last 30 days')).toHaveClass('text-text-tertiary')

    rerender(
      <StatTile label="Spend" value="$2" delta={{current: 2, prev: null, goodWhen: 'neutral', periodLabel: 'x'}} />,
    )
    expect(screen.queryByText(/vs last/)).not.toBeInTheDocument()
  })

  it('says why a value is missing instead of showing a zero', () => {
    render(<StatTile label="Avg health" value="0" unavailable="Not analyzed yet" />)
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.getByText('Not analyzed yet')).toBeInTheDocument()
    expect(screen.queryByText('0')).not.toBeInTheDocument()
  })

  it('is a button when clickable and renders a meter when given one', () => {
    const onClick = vi.fn()
    render(
      <StatTile
        label="Auto-fixable"
        value="98%"
        onClick={onClick}
        meter={{value: 98, max: 100, label: 'Auto-fixable share'}}
      />,
    )
    fireEvent.click(screen.getByRole('button', {name: /Auto-fixable, 98%/}))
    expect(onClick).toHaveBeenCalled()
    expect(screen.getByRole('meter', {name: 'Auto-fixable share'})).toHaveAttribute('aria-valuenow', '98')
  })

  it('uses the hero size for the one number a view leads with', () => {
    render(<StatTile label="Avg health" value="72" hero />)
    expect(screen.getByText('72')).toHaveClass('text-[48px]')
  })
})
