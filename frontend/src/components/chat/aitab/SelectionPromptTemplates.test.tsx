import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {SelectionPromptTemplates} from './SelectionPromptTemplates'
import {useFlowStore} from '@/stores/flowStore'
import type {Block, FlowDocument} from '@/types'

function block(id: string, name: string, rawType: string, lineNumber: number): Block {
  return {
    id,
    name,
    type: 'ACTION',
    rawType,
    indent: 0,
    lineNumber,
    children: [],
    properties: {},
    variables: [],
    subflowId: 'sf1',
  }
}

const doc = {
  id: 'doc1',
  name: 'Flow',
  subflows: [
    {
      id: 'sf1',
      name: 'Main',
      blocks: [block('b1', 'Fetch orders', 'HTTPClient.InvokeUrl', 2), block('b2', 'Post invoice', 'Excel.Write', 3)],
      variables: [],
    },
  ],
  variables: [],
} as unknown as FlowDocument

function renderTemplates(contextBlockId: string | null) {
  render(<SelectionPromptTemplates contextBlockId={contextBlockId} onSelect={vi.fn()} flowName="Flow" />)
  fireEvent.click(screen.getByRole('button', {name: /Templates/}))
}

beforeEach(() => {
  useFlowStore.getState().reset()
  useFlowStore.getState().setDocument(doc)
})

// R-frontend-7: the templates were labelled with the canvas selection while the request is grounded on the
// thread's context block.
describe('SelectionPromptTemplates', () => {
  it("labels the block template with the thread's context block, not the canvas selection", () => {
    useFlowStore.getState().selectBlock('b2')
    renderTemplates('b1')
    expect(screen.getByRole('button', {name: 'Explain "Fetch orders" in detail'})).toBeInTheDocument()
    expect(screen.queryByText(/Post invoice/)).not.toBeInTheDocument()
  })

  it('offers no block template for a selection alone (the thread has no context block)', () => {
    useFlowStore.getState().selectBlock('b2')
    renderTemplates(null)
    expect(screen.queryByText(/Explain .* in detail/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Explain entire flow'})).toBeInTheDocument()
  })

  it('offers no block template, and never a raw id, for a context block no longer in the flow', () => {
    renderTemplates('3f8a1c52-0000-4000-8000-000000000000')
    expect(screen.queryByText(/3f8a1c52/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Explain .* in detail/)).not.toBeInTheDocument()
  })
})
