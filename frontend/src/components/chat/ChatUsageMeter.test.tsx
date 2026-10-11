import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import ChatUsageMeter, {threadCost, threadCostEstimate} from './ChatUsageMeter'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import type {ChatMessage} from '@/types'

function assistant(partial: Partial<ChatMessage>): ChatMessage {
  return {id: partial.id ?? 'a1', role: 'assistant', content: 'x', timestamp: '2024-01-01T00:00:00Z', ...partial}
}

function seed(messages: ChatMessage[], showCost = true) {
  useChatStore.setState({activeThreadId: 'threadA', streams: {}, conversations: new Map([['threadA', messages]])})
  useSettingsStore.setState(s => ({settings: {...s.settings, ai: {...s.settings.ai, showCostEstimates: showCost}}}))
}

beforeEach(() => {
  seed([])
})

describe('threadCost', () => {
  it('prices everything at the listing when no turn carries its own rates', () => {
    expect(threadCost([assistant({tokensIn: 1000, tokensOut: 500})], 1000, 500, 2, 10)).toBeCloseTo(0.007, 9)
  })

  it('prices rated turns at their billed rates and only the remainder at the listing', () => {
    const msgs = [
      assistant({id: 'a1', tokensIn: 1000, tokensOut: 1000, inputCostPerM: 1, outputCostPerM: 4}),
      assistant({id: 'a2', tokensIn: 1000, tokensOut: 1000}),
      {id: 'u1', role: 'user', content: 'q', timestamp: 't', tokensIn: 5, inputCostPerM: 99} as ChatMessage,
    ]
    expect(threadCost(msgs, 2000, 2000, 2, 10)).toBeCloseTo(0.017, 9)
  })

  it('prices rated turns even when the listing is unknown, and hides when nothing can be priced', () => {
    const rated = [assistant({tokensIn: 2_000_000, tokensOut: 0, inputCostPerM: 1, outputCostPerM: 0})]
    expect(threadCost(rated, 2_000_000, 0, 0, 0)).toBe(2)
    expect(threadCost([assistant({tokensIn: 10, tokensOut: 10})], 10, 10, 0, 0)).toBeNull()
    expect(threadCost([assistant({tokensIn: 10, tokensOut: 10, inputCostPerM: -1})], 10, 10, 0, 0)).toBeNull()
  })

  it('never prices the listing remainder below zero when rated turns exceed the totals', () => {
    const rated = [assistant({tokensIn: 5000, tokensOut: 5000, inputCostPerM: 1, outputCostPerM: 1})]
    expect(threadCost(rated, 1000, 1000, 100, 100)).toBeCloseTo(0.01, 9)
  })
})

describe('threadCostEstimate', () => {
  const rated = assistant({id: 'a1', tokensIn: 1000, tokensOut: 0, inputCostPerM: 1, outputCostPerM: 1})
  it('is exact when every token is priced', () => {
    expect(threadCostEstimate([rated], 1000, 0, 0, 0)?.lowerBound).toBe(false)
    expect(threadCostEstimate([rated], 5000, 0, 1, 1)?.lowerBound).toBe(false)
  })

  it('is a lower bound when unrated tokens exist and the listing is $0', () => {
    expect(threadCostEstimate([rated, assistant({id: 'a2', tokensIn: 10})], 1010, 0, 0, 0)).toEqual({
      cost: 0.001,
      lowerBound: true,
    })
  })

  const azure = assistant({id: 'az', provider: 'azure-foundry', tokensIn: 3000, tokensOut: 1000})
  const openai = assistant({id: 'oa', provider: 'openai', tokensIn: 1000, tokensOut: 500})

  it('prices only the non-Azure tokens of a mixed thread at the current listing', () => {
    expect(threadCostEstimate([azure, openai], 4000, 1500, 2, 10)).toEqual({cost: 0.007, lowerBound: false})
  })

  it('shows no figure when every priced-looking token is Azure', () => {
    expect(threadCostEstimate([azure], 3000, 1000, 2, 10)).toBeNull()
    expect(threadCostEstimate([azure, rated], 4000, 1000, 0, 0)).toEqual({cost: 0.001, lowerBound: false})
  })
})

describe('ChatUsageMeter', () => {
  it('shows the cost from the listing price', () => {
    render(<ChatUsageMeter promptTokens={1000} completionTokens={500} inputCostPerM={2} outputCostPerM={10} />)
    expect(screen.getByText(/· \$0\.007/)).toBeInTheDocument()
  })

  it("uses a turn's billed rates over the listing price", () => {
    seed([assistant({tokensIn: 1000, tokensOut: 500, inputCostPerM: 20, outputCostPerM: 100})])
    render(<ChatUsageMeter promptTokens={1000} completionTokens={500} inputCostPerM={2} outputCostPerM={10} />)
    expect(screen.getByText(/· \$0\.070/)).toBeInTheDocument()
  })

  it('shows a cost from billed rates alone when the listing has none', () => {
    seed([assistant({tokensIn: 1000, tokensOut: 500, inputCostPerM: 20, outputCostPerM: 100})])
    render(<ChatUsageMeter promptTokens={1000} completionTokens={500} />)
    expect(screen.getByText(/· \$0\.070/)).toBeInTheDocument()
  })

  it('marks the cost as a lower bound when some turns have no known price and the listing is $0', () => {
    seed([
      assistant({id: 'a1', tokensIn: 1000, tokensOut: 500, inputCostPerM: 20, outputCostPerM: 100}),
      assistant({id: 'a2', tokensIn: 1000, tokensOut: 500}),
    ])
    render(<ChatUsageMeter promptTokens={2000} completionTokens={1000} inputCostPerM={0} outputCostPerM={0} />)
    expect(screen.getByText(/≥ \$0\.070/)).toBeInTheDocument()
    expect(screen.getByTitle(/At least this much/)).toBeInTheDocument()
  })

  it('hides the cost when no rate is known, or when cost estimates are off', () => {
    const {unmount} = render(<ChatUsageMeter promptTokens={1000} completionTokens={500} />)
    expect(screen.getByText(/tokens/)).toBeInTheDocument()
    expect(screen.queryByText(/\$/)).toBeNull()
    unmount()

    seed([assistant({tokensIn: 1000, tokensOut: 500, inputCostPerM: 20, outputCostPerM: 100})], false)
    render(<ChatUsageMeter promptTokens={1000} completionTokens={500} inputCostPerM={2} outputCostPerM={10} />)
    expect(screen.queryByText(/\$/)).toBeNull()
  })

  it('does not price Azure turns at the current model listing in a mixed thread', () => {
    seed([
      assistant({id: 'a1', provider: 'azure-foundry', tokensIn: 100_000, tokensOut: 50_000}),
      assistant({id: 'a2', provider: 'openai', tokensIn: 1000, tokensOut: 500}),
    ])
    render(<ChatUsageMeter promptTokens={101_000} completionTokens={50_500} inputCostPerM={2} outputCostPerM={10} />)
    expect(screen.getByText(/· \$0\.007/)).toBeInTheDocument()
  })

  it('shows only token counts for an unpriced (Azure) thread: zero rates on every turn and a $0 listing', () => {
    seed([
      assistant({id: 'a1', tokensIn: 1000, tokensOut: 500, inputCostPerM: 0, outputCostPerM: 0, servedModel: 'gpt-4o'}),
      assistant({id: 'a2', tokensIn: 200, tokensOut: 50, inputCostPerM: 0, outputCostPerM: 0, servedModel: 'gpt-4o'}),
    ])
    render(<ChatUsageMeter promptTokens={1200} completionTokens={550} inputCostPerM={0} outputCostPerM={0} />)
    expect(screen.getByText(/tokens/)).toBeInTheDocument()
    expect(screen.queryByText(/\$/)).toBeNull()
    expect(screen.queryByTitle(/At least this much/)).toBeNull()
  })
})
