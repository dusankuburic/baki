import {describe, it, expect, vi} from 'vitest'
import {render, screen, within} from '@testing-library/react'
import {mockDashboardAnalytics} from '@/testing/fixtures/dashboardAnalytics'
import type {DashboardAnalyticsData} from '@/types'
import {AiSection, modelLabel} from './AiSection'

vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => (
      <div style={{width: 600, height: 240}}>{children}</div>
    ),
  }
})

// D-42: usage rows carry the model that actually answered next to what was
// asked for. The by-model table labels a row "deployment · served" only when
// they differ, so plain providers read exactly as before.
describe('AiSection model breakdown', () => {
  it('labels a row "deployment · served" when the served model differs', () => {
    const data: DashboardAnalyticsData = mockDashboardAnalytics()
    data.ai.byModel = [
      {provider: 'azure-foundry', model: 'model-router', servedModel: 'gpt-4o', tokensIn: 100, tokensOut: 10, calls: 2, cost: 1},
      {provider: 'azure-foundry', model: 'model-router', servedModel: 'gpt-4.1-mini', tokensIn: 50, tokensOut: 5, calls: 1, cost: 0.1},
      {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15, cost: 1.02},
    ]
    render(<AiSection data={data} refetching={false} />)

    const table = screen.getByRole('table', {name: 'Usage by model'})
    const rows = within(table).getAllByRole('row').slice(1) // header row first
    expect(rows).toHaveLength(3)
    expect(within(rows[0]).getByText('model-router · gpt-4o')).toBeInTheDocument()
    expect(within(rows[1]).getByText('model-router · gpt-4.1-mini')).toBeInTheDocument()
    expect(within(rows[2]).getByText('gpt-5')).toBeInTheDocument()
    expect(within(rows[2]).queryByText(/·/)).toBeNull()
  })

  it('modelLabel falls back to the requested id when served is absent or identical', () => {
    expect(modelLabel({model: 'gpt-5'})).toBe('gpt-5')
    expect(modelLabel({model: 'gpt-4o', servedModel: 'gpt-4o'})).toBe('gpt-4o')
    expect(modelLabel({model: 'agent:support-bot', servedModel: 'gpt-4o-2024-11-20'})).toBe(
      'agent:support-bot · gpt-4o-2024-11-20',
    )
  })
})
