import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import {SelectionContextBar} from './SelectionContextBar'
import {useFlowStore} from '@/stores/flowStore'
import type {Block, FlowDocument} from '@/types'

function block(id: string, name: string, lineNumber: number): Block {
  return {
    id,
    name,
    type: 'ACTION',
    rawType: 'HTTPClient.InvokeUrl',
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
      blocks: [block('b1', 'Fetch orders', 2), {...block('b2', 'Post invoice', 3), rawType: 'Excel.WriteToExcel'}],
      variables: [],
    },
  ],
  variables: [],
} as unknown as FlowDocument

const noop = () => {}

function renderBar(props: {contextBlockId: string | null; lostBlock?: string}) {
  return render(
    <SelectionContextBar
      {...props}
      onClearBlock={noop}
      onFocusBlock={noop}
      files={[]}
      selectedFiles={[]}
      onFilesChange={noop}
    />,
  )
}

beforeEach(() => {
  useFlowStore.getState().reset()
  useFlowStore.getState().setDocument(doc)
})

// R-analyzer-boundary-2: the chip named the block selected on the canvas, not the thread's context block, and
// fell back to the raw id once a fix cleared the selection.
describe('SelectionContextBar', () => {
  it('names the context block, not the canvas selection', () => {
    useFlowStore.getState().selectBlock('b2')
    renderBar({contextBlockId: 'b1'})
    expect(screen.getByText('Fetch orders')).toBeInTheDocument()
    expect(screen.queryByText('Post invoice')).not.toBeInTheDocument()
  })

  it("gives the context block's type, not the selection's (R-frontend-7)", () => {
    useFlowStore.getState().selectBlock('b2')
    renderBar({contextBlockId: 'b1'})
    expect(screen.getByText('Fetch orders')).toHaveAttribute('title', 'Fetch orders (HTTPClient.InvokeUrl)')
  })

  it('never shows a raw id for a context block that is not in the flow', () => {
    renderBar({contextBlockId: '3f8a1c52-0000-4000-8000-000000000000'})
    expect(screen.queryByText(/3f8a1c52/)).not.toBeInTheDocument()
    expect(screen.getByText('Block not in the flow')).toBeInTheDocument()
  })

  it('says which block went when a fix removed it', () => {
    renderBar({contextBlockId: null, lostBlock: 'Post invoice'})
    expect(screen.getByRole('status')).toHaveTextContent('"Post invoice" is no longer in the flow')
  })

  it('still says a block went when its name is unknown (a thread from before anchors)', () => {
    renderBar({contextBlockId: null, lostBlock: ''})
    expect(screen.getByRole('status')).toHaveTextContent('The block this chat was about is no longer in the flow')
    expect(screen.queryByText(/""/)).not.toBeInTheDocument()
  })
})
