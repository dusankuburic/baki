import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, within} from '@testing-library/react'
import {useSettingsStore} from '@/stores/settingsStore'
import {AiUsageCard} from './AiUsageCard'

vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  const {cloneElement} = await import('react')
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactElement<{width?: number; height?: number}>}) =>
      cloneElement(children, {width: 600, height: 240}),
  }
})

const tokens = [{date: '2026-09-28', tokensIn: 1200, tokensOut: 300}]

beforeEach(() => {
  useSettingsStore.setState(s => ({
    settings: {...s.settings, appearance: {...s.settings.appearance, reduceMotion: true}},
  }))
})

describe('AiUsageCard cost by provider', () => {
  it('leaves Azure AI Foundry out of the cost chart', () => {
    render(
      <AiUsageCard
        tokens={tokens}
        cost={[
          {provider: 'azure-foundry', cost: 0, tokensIn: 150, tokensOut: 15},
          {provider: 'openai', cost: 1.02, tokensIn: 12000, tokensOut: 4000},
        ]}
        isCloud
      />,
    )
    const figure = screen.getByText('Cost by provider · last 30 days').parentElement!
    const texts = (selector: string) => Array.from(figure.querySelectorAll(selector), n => n.textContent)
    expect(texts('.recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value')).toEqual(['openai'])
    expect(texts('.recharts-label-list .recharts-label')).toEqual(['$1.02'])
  })

  it('says Azure is not priced instead of drawing a $0.00 bar or "no data"', () => {
    render(
      <AiUsageCard
        tokens={tokens}
        cost={[{provider: 'azure-foundry', cost: 0, tokensIn: 150, tokensOut: 15}]}
        isCloud
      />,
    )
    const card = screen.getByRole('region', {name: 'AI usage'})
    expect(within(card).getByText('Only Azure AI Foundry was used, and baki does not price it.')).toBeInTheDocument()
    expect(within(card).queryByText('No data yet.')).toBeNull()
    expect(card.querySelector('.recharts-label-list')).toBeNull()
  })
})
