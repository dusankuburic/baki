import {describe, it, expect} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {ToolTrail, FixOutcomeStrip} from './ToolTrail'
import type {ToolCallRecord, FixProposalSnapshot} from '@/types'

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
    // Details hidden until expanded.
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

  it('uses singular "tool" for one call', () => {
    render(<ToolTrail calls={[calls[0]]} />)
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
