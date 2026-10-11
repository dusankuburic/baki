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
        cost: 0,
      },
      {
        provider: 'azure-foundry',
        model: 'model-router',
        servedModel: 'gpt-4.1-mini',
        tokensIn: 50,
        tokensOut: 5,
        calls: 1,
        cost: 0,
      },
      {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15, cost: 1.02},
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

describe('AiSection unpriced Azure AI Foundry rows', () => {
  beforeEach(() => downloadBlob.mockClear())

  function azureData(): DashboardAnalyticsData {
    const data: DashboardAnalyticsData = mockDashboardAnalytics()
    data.ai.byProvider = [
      {provider: 'azure-foundry', cost: 0, tokensIn: 150, tokensOut: 15},
      {provider: 'openai', cost: 1.02, tokensIn: 12000, tokensOut: 4000},
    ]
    data.ai.byModel = [
      {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15, cost: 1.02},
      {
        provider: 'azure-foundry',
        model: 'model-router',
        servedModel: 'gpt-4o',
        tokensIn: 150,
        tokensOut: 15,
        calls: 3,
        cost: 0,
      },
    ]
    return data
  }

  it('shows an em dash for Azure in the by-model table and keeps the number in the CSV', () => {
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Usage by model'})
    const rows = within(within(card).getByRole('table', {name: 'Usage by model'}))
      .getAllByRole('row')
      .slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('$1.02')).toBeInTheDocument()
    expect(within(rows[0]!).queryByText('—')).toBeNull()
    expect(within(rows[1]!).getByText('—')).toBeInTheDocument()
    expect(within(rows[1]!).queryByText(/\$/)).toBeNull()

    fireEvent.click(within(card).getByRole('button', {name: 'Download CSV'}))
    expect(downloadBlob).toHaveBeenCalledTimes(1)
    const [content] = downloadBlob.mock.calls[0] as [string]
    expect(content).not.toContain('—')
    const azureLine = content.split('\n').find(l => l.startsWith('azure-foundry'))
    expect(azureLine).toBeDefined()
    expect(azureLine?.endsWith(',0')).toBe(true)
  })

  it('shows an em dash for Azure in the by-provider table view', () => {
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Cost by provider'})
    const toggle = within(card).getByRole('radiogroup', {name: 'View Cost by provider as'})
    fireEvent.click(within(toggle).getByRole('radio', {name: 'Table'}))

    const rows = within(within(card).getByRole('table', {name: 'Cost by provider'}))
      .getAllByRole('row')
      .slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('openai')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('$1.02')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('azure-foundry')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('—')).toBeInTheDocument()
    expect(within(rows[1]!).queryByText(/\$/)).toBeNull()
  })

  it('names the dash "Not priced by baki" for assistive tech', () => {
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Usage by model'})
    const table = within(card).getByRole('table', {name: 'Usage by model'})
    const cell = within(table).getByRole('cell', {name: 'Not priced by baki'})
    expect(cell).toHaveTextContent('—')
    expect(within(cell).getByTitle('Not priced by baki')).toBeInTheDocument()
  })

  it('leaves Azure out of the cost-by-provider chart (no $0.00 bar label)', () => {
    useSettingsStore.setState(s => ({
      settings: {...s.settings, appearance: {...s.settings.appearance, reduceMotion: true}},
    }))
    render(<AiSection data={azureData()} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Cost by provider'})
    const texts = (selector: string) => Array.from(card.querySelectorAll(selector), n => n.textContent)
    expect(texts('.recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value')).toEqual(['openai'])
    expect(texts('.recharts-label-list .recharts-label')).toEqual(['$1.02'])
  })

  it('explains an Azure-only cost-by-provider chart instead of drawing a $0.00 bar', () => {
    const data = azureData()
    data.ai.byProvider = [{provider: 'azure-foundry', cost: 0, tokensIn: 150, tokensOut: 15}]
    render(<AiSection data={data} refetching={false} />)

    const card = screen.getByRole('region', {name: 'Cost by provider'})
    expect(within(card).getByText('Only Azure AI Foundry was used, and baki does not price it.')).toBeInTheDocument()
    expect(within(card).queryByText('$0.00')).toBeNull()
  })
})
