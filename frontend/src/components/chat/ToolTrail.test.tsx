import {describe, it, expect} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {ToolTrail, FixOutcomeStrip, ApprovalOutcomeStrip} from './ToolTrail'
import type {ApprovalSnapshot, ToolCallRecord, FixProposalSnapshot} from '@/types'

const calls: ToolCallRecord[] = [
  {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 12, summary: '3 matches for "xav"'},
  {name: 'apply_fix', label: 'Applying fix', ok: false, durationMs: 60002, summary: 'error: no decision'},
]

describe('ToolTrail', () => {
  it('renders nothing for an empty trail', () => {
    const {container} = render(<ToolTrail calls={[]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows a collapsed summary with the call count and failure count', () => {
    render(<ToolTrail calls={calls} />)
    expect(screen.getByText(/Used 2 tools/)).toBeInTheDocument()
    expect(screen.getByText(/1 failed/)).toBeInTheDocument()
    expect(screen.queryByText('Searching flow')).not.toBeInTheDocument()
  })

  it('expands to per-call rows and collapses back', () => {
    render(<ToolTrail calls={calls} />)
    fireEvent.click(screen.getByRole('button'))
    expect(screen.getByText('Searching flow')).toBeInTheDocument()
    expect(screen.getByText('Applying fix')).toBeInTheDocument()
    expect(screen.getByText(/3 matches for "xav"/)).toBeInTheDocument()
    expect(screen.getByText(/60\.0 s/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button'))
    expect(screen.queryByText('Searching flow')).not.toBeInTheDocument()
  })

  // The trail names a call by the server's label, with no closed map of tool names: a tool the client has never heard
  // of (search_knowledge, R-knowledge) reads as the server words it, and one without a label by its name.
  it("names a call by the server's label, so a new tool needs no client change", () => {
    render(
      <ToolTrail
        calls={[
          {name: 'search_knowledge', label: 'Searching knowledge base', ok: true, summary: '2 passages'},
          {name: 'search_knowledge', ok: true},
        ]}
      />,
    )
    fireEvent.click(screen.getByRole('button'))
    expect(screen.getByText('Searching knowledge base')).toBeInTheDocument()
    expect(screen.getByText('search_knowledge')).toBeInTheDocument()
  })

  it('uses singular "tool" for one call', () => {
    render(<ToolTrail calls={[calls[0]!]} />)
    expect(screen.getByText(/Used 1 tool/)).toBeInTheDocument()
    expect(screen.queryByText(/failed/)).not.toBeInTheDocument()
  })
})

describe('FixOutcomeStrip', () => {
  const base: FixProposalSnapshot = {
    proposalId: 'p1',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'applied',
    message: 'finding resolved',
  }

  it('renders the fix type, block, and applied outcome', () => {
    render(<FixOutcomeStrip snapshot={base} />)
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('wrap-error-handler')
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('Call API')
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('Fix applied and verified')
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('finding resolved')
  })

  it('renders declined without a message', () => {
    render(<FixOutcomeStrip snapshot={{...base, status: 'declined', message: undefined}} />)
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('Declined — nothing was changed')
  })

  it('falls back to the raw status for unknown values', () => {
    render(<FixOutcomeStrip snapshot={{...base, status: 'mystery'}} />)
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('mystery')
  })

  it('renders batch outcomes as a per-fix row list', () => {
    render(
      <FixOutcomeStrip
        snapshot={{
          ...base,
          status: 'applied-unresolved',
          items: [
            {...base, status: 'applied', blockLabel: 'Call API'},
            {...base, status: 'applied-unresolved', blockLabel: 'Sync API', message: 'still appears'},
          ],
        }}
      />,
    )
    const strip = screen.getByTestId('fix-outcome')
    expect(strip).toHaveTextContent('2 fixes')
    expect(strip).toHaveTextContent('still appears')
    expect(strip).toHaveTextContent('Call API')
    expect(strip).toHaveTextContent('Sync API')
    expect(strip).toHaveTextContent('still appears')
  })
})

describe('FixOutcomeStrip by previewKind', () => {
  const base: FixProposalSnapshot = {
    proposalId: 'p1',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'applied',
  }

  it('a persisted proposal without previewKind renders as fix-patch', () => {
    render(<FixOutcomeStrip snapshot={base} />)
    const strip = screen.getByTestId('fix-outcome')
    expect(strip).toHaveAttribute('data-preview-kind', 'fix-patch')
    expect(strip).toHaveTextContent('wrap-error-handler')
    expect(screen.queryByTestId('approval-outcome')).not.toBeInTheDocument()
  })

  it('a persisted approval with an unknown previewKind renders the generic strip', () => {
    render(
      <FixOutcomeStrip
        snapshot={{...base, tool: 'rename_variable', previewKind: 'something-new', message: 'renamed'}}
      />,
    )
    expect(screen.queryByTestId('fix-outcome')).not.toBeInTheDocument()
    const strip = screen.getByTestId('approval-outcome')
    expect(strip).toHaveAttribute('data-preview-kind', 'something-new')
    expect(strip).toHaveTextContent('rename_variable')
    expect(strip).toHaveTextContent('Call API')
    expect(strip).not.toHaveTextContent('Fix applied')
    expect(strip).toHaveTextContent('Done.')
    expect(strip).toHaveTextContent('renamed')
  })
})

describe('ApprovalOutcomeStrip', () => {
  const approval: ApprovalSnapshot = {
    proposalId: 'a1',
    tool: 'rename_variable',
    previewKind: 'something-new',
    requiredScope: 'write',
    status: 'applied-unresolved',
    message: 'one failed',
    items: [
      {id: 'v1', label: 'Rename x', summary: 'x → total', status: 'applied'},
      {id: 'v2', label: 'Rename y', summary: 'y → count', status: 'error', message: 'in use'},
    ],
  }

  it('renders the tool, the overall outcome and a per-item row list for a batch', () => {
    render(<ApprovalOutcomeStrip snapshot={approval} />)
    const strip = screen.getByTestId('approval-outcome')
    expect(strip).toHaveTextContent('rename_variable')
    expect(strip).toHaveTextContent('Done — review recommended.')
    expect(strip).toHaveTextContent('one failed')
    expect(strip).toHaveTextContent('Rename x')
    expect(strip).toHaveTextContent('done')
    expect(strip).toHaveTextContent('Rename y')
    expect(strip).toHaveTextContent('failed')
    expect(strip).toHaveTextContent('in use')
  })

  it('names a single item inline and falls back to the raw status', () => {
    render(
      <ApprovalOutcomeStrip
        snapshot={{...approval, status: 'mystery', message: undefined, items: [approval.items[0]!]}}
      />,
    )
    const strip = screen.getByTestId('approval-outcome')
    expect(strip).toHaveTextContent('Rename x')
    expect(strip).toHaveTextContent('mystery')
    expect(strip.querySelector('ul')).toBeNull()
  })
})
