import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import ContextPreviewModal from './ContextPreviewModal'
import type {ContextPreview} from '@/types'

// The preview shows what the turn sends (R-analyzer-boundary-3): the estimate by part, the history the window
// would drop, the tools offered and the effective context budget, not just three texts and one number.
const preview: ContextPreview = {
  systemPrompt: 'system',
  contextText: 'context',
  userMessage: 'question',
  estimatedTokens: 1200,
  contextLimit: 8000,
  breakdown: {system: 300, context: 500, history: 250, tools: 100, user: 50},
  historyMessages: 6,
  historyDropped: 2,
  toolsOffered: true,
  tools: ['search_flow', 'get_block'],
  contextBudget: 4000,
}

describe('ContextPreviewModal', () => {
  it('shows the estimate by part, the history the window would drop, the tools and the budget', () => {
    render(<ContextPreviewModal preview={preview} onClose={() => {}} onConfirm={() => {}} />)
    expect(screen.getByText(/System 300/)).toBeInTheDocument()
    expect(screen.getByText(/History 250/)).toBeInTheDocument()
    expect(screen.getByText(/Tools 100/)).toBeInTheDocument()
    expect(screen.getByText(/2 of 6 earlier messages/)).toBeInTheDocument()
    expect(screen.getByText(/search_flow, get_block/)).toBeInTheDocument()
    expect(screen.getByText(/4,000/)).toBeInTheDocument()
  })

  it('says nothing about dropped history or tools when there are none', () => {
    render(
      <ContextPreviewModal
        preview={{...preview, historyDropped: 0, toolsOffered: false, tools: undefined}}
        onClose={() => {}}
        onConfirm={() => {}}
      />,
    )
    expect(screen.queryByText(/earlier messages/)).toBeNull()
    expect(screen.queryByText(/search_flow/)).toBeNull()
  })
})
