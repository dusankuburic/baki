import {beforeEach, describe, it, expect, vi} from 'vitest'
import {fireEvent, render, screen, within} from '@testing-library/react'
import {mockDashboardAnalytics} from '@/testing/fixtures/dashboardAnalytics'
import i18n from '@/i18n'
import {useSettingsStore} from '@/stores/settingsStore'
import type {DashboardAnalyticsData} from '@/types'
import {AiSection, modelLabel} from './AiSection'

vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  const {cloneElement} = await import('react')
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactElement<{width?: number; height?: number}>}) =>
      cloneElement(children, {width: 600, height: 240}),
  }
})

const downloadBlob = vi.fn()
vi.mock('@/lib/csv', async importOriginal => {
  const actual = await importOriginal<typeof import('@/lib/csv')>()
  return {...actual, downloadBlob: (...a: unknown[]) => downloadBlob(...a)}
})

describe('AiSection model breakdown', () => {
  it('labels a row "deployment · served" when the served model differs', () => {
    const data: DashboardAnalyticsData = mockDashboardAnalytics()
    data.ai.byModel = [
      {
        provider: 'azure-foundry',
        model: 'model-router',
        servedModel: 'gpt-4o',
        tokensIn: 100,
        tokensOut: 10,
        calls: 2,
      },
      {
        provider: 'azure-foundry',
        model: 'model-router',
        servedModel: 'gpt-4.1-mini',
        tokensIn: 50,
        tokensOut: 5,
        calls: 1,
      },
      {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15},
    ]
    render(<AiSection data={data} refetching={false} />)

    const table = screen.getByRole('table', {name: 'Usage by model'})
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(3)
    expect(within(rows[0]!).getByText('model-router · gpt-4o')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('model-router · gpt-4.1-mini')).toBeInTheDocument()
    expect(within(rows[2]!).getByText('gpt-5')).toBeInTheDocument()
    expect(within(rows[2]!).queryByText(/·/)).toBeNull()
  })

  it('modelLabel falls back to the requested id when served is absent or identical', () => {
    const t = i18n.getFixedT('en', 'dashboard')
    expect(modelLabel(t, {model: 'gpt-5'})).toBe('gpt-5')
    expect(modelLabel(t, {model: 'gpt-4o', servedModel: 'gpt-4o'})).toBe('gpt-4o')
    expect(modelLabel(t, {model: 'agent:support-bot', servedModel: 'gpt-4o-2024-11-20'})).toBe(
      'Agent · support-bot · gpt-4o-2024-11-20',
    )
    expect(modelLabel(t, {model: 'claude:prod', servedModel: 'claude-opus-5-5'})).toBe(
      'prod · Claude · claude-opus-5-5',
    )
    expect(modelLabel(t, {model: 'claude:prod'})).toBe('prod · Claude')
    expect(modelLabel(t, {model: 'my-deployment'})).toBe('my-deployment')
  })

  it('modelLabel names the routing prefixes through the translation, not hard-coded English', () => {
    const t = ((key: string, opts: {name: string}) => `${key}(${opts.name})`) as unknown as Parameters<
      typeof modelLabel
    >[0]
    expect(modelLabel(t, {model: 'agent:bot'})).toBe('analytics.models.agent(bot)')
    expect(modelLabel(t, {model: 'claude:prod', servedModel: 'claude-x'})).toBe(
      'analytics.models.claude(prod) · claude-x',
    )
  })
})

describe('AiSection provider and model token telemetry', () => {
  beforeEach(() => downloadBlob.mockClear())

  function azureData(): DashboardAnalyticsData {
    const data: DashboardAnalyticsData = mockDashboardAnalytics()
    data.ai.byProvider = [
      {provider: 'azure-foundry', tokensIn: 150, tokensOut: 15},
      {provider: 'openai', tokensIn: 12000, tokensOut: 4000},
    ]
    data.ai.byModel = [
      {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15},
      {
        provider: 'azure-foundry',
        model: 'model-router',
        servedModel: 'gpt-4o',
        tokensIn: 150,
        tokensOut: 15,
        calls: 3,
      },
    ]
    return data
  }

  it('renders model token usage and exports CSV without monetary values', () => {
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Usage by model'})
    const rows = within(within(card).getByRole('table', {name: 'Usage by model'}))
      .getAllByRole('row')
      .slice(1)
    expect(rows).toHaveLength(2)
    // First row: openai, gpt-5, 15 calls, 12K in, 4K out
    expect(within(rows[0]!).getByText('openai')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('gpt-5')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('15')).toBeInTheDocument()
    // No dollar signs rendered anywhere in table
    expect(within(card).queryByText(/\$/)).toBeNull()

    fireEvent.click(within(card).getByRole('button', {name: 'Download CSV'}))
    expect(downloadBlob).toHaveBeenCalledTimes(1)
    const [content] = downloadBlob.mock.calls[0] as [string]
    expect(content).not.toContain('$')
    const openaiLine = content.split('\n').find(l => l.startsWith('openai'))
    expect(openaiLine).toBeDefined()
    expect(openaiLine).toContain('12000')
    expect(openaiLine).toContain('4000')
  })

  it('shows token breakdown for providers in table view', () => {
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Cost by provider'})
    const toggle = within(card).getByRole('radiogroup', {name: 'View Cost by provider as'})
    fireEvent.click(within(toggle).getByRole('radio', {name: 'Table'}))

    const rows = within(within(card).getByRole('table', {name: 'Cost by provider'}))
      .getAllByRole('row')
      .slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('openai')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('azure-foundry')).toBeInTheDocument()
    expect(within(card).queryByText(/\$/)).toBeNull()
  })

  it('renders provider token chart without dollar signs', () => {
    useSettingsStore.setState(s => ({
      settings: {...s.settings, appearance: {...s.settings.appearance, reduceMotion: true}},
    }))
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Cost by provider'})
    const texts = (selector: string) => Array.from(card.querySelectorAll(selector), n => n.textContent)
    expect(texts('.recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value')).toContain('openai')
    expect(texts('.recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value')).toContain('azure-foundry')
    expect(within(card).queryByText(/\$/)).toBeNull()
  })
})
