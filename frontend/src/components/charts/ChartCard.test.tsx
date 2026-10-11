import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import {ChartCard} from './ChartCard'

const table = {
  caption: 'Findings by rule',
  columns: [
    {key: 'rule', header: 'Rule'},
    {key: 'count', header: 'Findings', align: 'right' as const},
  ],
  rows: [
    {rule: 'missing-timeout', count: 12},
    {rule: 'unused-variable', count: 3},
  ],
  rowKey: (r: {rule: string; count: number}) => r.rule,
}

describe('ChartCard', () => {
  it('names the card by its title and wraps the chart body as a labelled group', () => {
    render(
      <ChartCard title="Health trend" subtitle="Last 30 days">
        <svg data-testid="chart" />
      </ChartCard>,
    )
    expect(screen.getByRole('region', {name: 'Health trend'})).toBeInTheDocument()
    expect(screen.getByText('Last 30 days')).toBeInTheDocument()
    expect(screen.getByRole('group', {name: 'Health trend'})).toContainElement(screen.getByTestId('chart'))
  })

  it('switches to the table twin and back', () => {
    render(
      <ChartCard title="Findings by rule" table={table}>
        <svg data-testid="chart" />
      </ChartCard>,
    )
    const toggle = screen.getByRole('radiogroup', {name: 'View Findings by rule as'})
    fireEvent.click(within(toggle).getByRole('radio', {name: 'Table'}))
    const grid = screen.getByRole('table', {name: 'Findings by rule'})
    expect(within(grid).getAllByRole('row')).toHaveLength(3)
    expect(within(grid).getByText('missing-timeout')).toBeInTheDocument()
    expect(screen.queryByTestId('chart')).not.toBeInTheDocument()
    fireEvent.click(within(toggle).getByRole('radio', {name: 'Chart'}))
    expect(screen.getByTestId('chart')).toBeInTheDocument()
  })

  it('marks a refetch as busy and keeps the previous chart mounted', () => {
    render(
      <ChartCard title="Health trend" refetching>
        <svg data-testid="chart" />
      </ChartCard>,
    )
    expect(screen.getByRole('region', {name: 'Health trend'})).toHaveAttribute('aria-busy', 'true')
    expect(screen.getByTestId('chart')).toBeInTheDocument()
  })

  it('shows the empty message, and an error with a retry', () => {
    const onRetry = vi.fn()
    const {rerender} = render(
      <ChartCard title="Health trend" state="empty" emptyMessage="Analyze a flow to see its health.">
        <svg data-testid="chart" />
      </ChartCard>,
    )
    expect(screen.getByText('Analyze a flow to see its health.')).toBeInTheDocument()
    expect(screen.queryByTestId('chart')).not.toBeInTheDocument()

    rerender(
      <ChartCard title="Health trend" state="error" errorMessage="boom" onRetry={onRetry}>
        <svg data-testid="chart" />
      </ChartCard>,
    )
    expect(screen.getByText('boom')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })
})
