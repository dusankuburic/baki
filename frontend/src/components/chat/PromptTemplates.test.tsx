import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import PromptTemplates from './PromptTemplates'

function open(hasBlock: boolean) {
  render(<PromptTemplates onSelect={vi.fn()} hasBlock={hasBlock} />)
  fireEvent.click(screen.getByRole('button', {name: /templates/i}))
}

describe('PromptTemplates', () => {
  it('hides the block template when no block is in scope', () => {
    open(false)
    // It asks about "this block" — sent without one, the model had nothing to explain.
    expect(screen.queryByText('Explain block in detail')).not.toBeInTheDocument()
    expect(screen.getByText('Explain entire flow')).toBeInTheDocument()
  })

  it('offers it once a block is in scope', () => {
    open(true)
    expect(screen.getByText('Explain block in detail')).toBeInTheDocument()
  })
})
