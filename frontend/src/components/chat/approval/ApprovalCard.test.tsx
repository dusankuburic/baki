import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import ApprovalCard from './ApprovalCard'
import type {ApprovalCard as CardState, ApprovalItem} from '@/stores/chatStore'

function fixItem(overrides: Partial<ApprovalItem> = {}): ApprovalItem {
  return {
    id: 'unhandled-error',
    label: 'Call API',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'pending',
    ...overrides,
  }
}

function pendingCard(overrides: Partial<CardState> = {}): CardState {
  return {
    proposalId: 'p1',
    tool: 'apply_fix',
    previewKind: 'fix-patch',
    source: 'fix',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'pending',
    items: [fixItem()],
    ...overrides,
  }
}

function batchCard(overrides: Partial<CardState> = {}): CardState {
  return {
    proposalId: 'batch-1',
    tool: 'apply_fixes',
    previewKind: 'fix-patch',
    source: 'fix',
    status: 'pending',
    ruleId: '',
    fixType: '',
    blockLabel: '',
    line: 0,
    summary: '',
    items: [
      fixItem({summary: 'wrap'}),
      fixItem({
        id: 'missing-retry',
        label: 'Sync API',
        ruleId: 'missing-retry',
        fixType: 'wrap-in-retry',
        blockLabel: 'Sync API',
        line: 8,
        summary: 'retry',
      }),
    ],
    ...overrides,
  }
}

// A request of a kind this bundle does not know (a tool a newer server
// added): only the generic fields are there.
function genericCard(overrides: Partial<CardState> = {}): CardState {
  return {
    proposalId: 'a1',
    tool: 'rename_variable',
    previewKind: 'something-new',
    source: 'approval',
    requiredScope: 'write',
    status: 'pending',
    items: [{id: 'v1', label: 'Variable %Customer%', summary: 'Rename to %CustomerRecord%', status: 'pending'}],
    ...overrides,
  }
}

describe('ApprovalCard (fix-patch)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders the pending approval prompt with buttons and countdown', () => {
    const onRespond = vi.fn()
    render(<ApprovalCard approval={pendingCard()} onRespond={onRespond} />)

    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    expect(screen.getByText('Approve & apply')).toBeInTheDocument()
    expect(screen.getByText('Dismiss')).toBeInTheDocument()
    expect(screen.getByText(/60s to decide/)).toBeInTheDocument()
  })

  it('fix-patch renders block, line, rule and patch preview', () => {
    render(<ApprovalCard approval={pendingCard()} onRespond={vi.fn()} />)
    expect(screen.getByRole('alertdialog', {name: 'Proposed fix: wrap-error-handler'})).toBeInTheDocument()
    expect(screen.getByText('Call API')).toBeInTheDocument()
    expect(screen.getByText(/line 3/)).toBeInTheDocument()
    expect(screen.getByText('unhandled-error')).toBeInTheDocument()
    expect(screen.getByText('wrap-error-handler')).toBeInTheDocument()
    expect(screen.getByText('wrap lines 3-3')).toBeInTheDocument()
  })

  // An older server sends no previewKind: the card is a fix card.
  it('renders a card without a preview kind as fix-patch', () => {
    render(<ApprovalCard approval={pendingCard({previewKind: ''})} onRespond={vi.fn()} />)
    expect(screen.getByText('Proposed fix — approval needed')).toBeInTheDocument()
    expect(screen.getByText('unhandled-error')).toBeInTheDocument()
  })

  it('counts down the decision window while pending', () => {
    render(<ApprovalCard approval={pendingCard()} onRespond={vi.fn()} />)
    act(() => {
      vi.advanceTimersByTime(15_000)
    })
    expect(screen.getByText(/45s to decide/)).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(50_000)
    })
    expect(screen.getByText(/expiring…/)).toBeInTheDocument()
  })

  it('keeps counting from when the proposal arrived across a remount', () => {
    const receivedAt = Date.now()
    const {unmount} = render(<ApprovalCard approval={pendingCard({receivedAt})} onRespond={vi.fn()} />)
    act(() => {
      vi.advanceTimersByTime(20_000)
    })
    unmount()
    // Thread switch-back / pop-out remounts the card; the server's window did
    // not restart, so neither may the countdown.
    render(<ApprovalCard approval={pendingCard({receivedAt})} onRespond={vi.fn()} />)
    expect(screen.getByText(/40s to decide/)).toBeInTheDocument()
  })

  it('stops the countdown once resolved', () => {
    const {rerender} = render(<ApprovalCard approval={pendingCard()} onRespond={vi.fn()} />)
    act(() => {
      vi.advanceTimersByTime(10_000)
    })
    rerender(<ApprovalCard approval={pendingCard({status: 'applied', message: 'verified'})} onRespond={vi.fn()} />)
    expect(screen.queryByText(/to decide/)).not.toBeInTheDocument()
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('Fix applied and verified')
  })

  it('reports Approve and Dismiss decisions', () => {
    const onRespond = vi.fn()
    const {rerender} = render(<ApprovalCard approval={pendingCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Approve & apply'))
    expect(onRespond).toHaveBeenCalledWith(true, 'p1', undefined)
    rerender(<ApprovalCard approval={pendingCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Dismiss'))
    expect(onRespond).toHaveBeenCalledWith(false, 'p1')
  })

  it('Escape dismisses while pending', () => {
    const onRespond = vi.fn()
    render(<ApprovalCard approval={pendingCard()} onRespond={onRespond} />)
    fireEvent.keyDown(screen.getByRole('alertdialog'), {key: 'Escape'})
    expect(onRespond).toHaveBeenCalledWith(false, 'p1')
  })

  it('moves focus to the card when the prompt appears', () => {
    render(<ApprovalCard approval={pendingCard()} onRespond={vi.fn()} />)
    expect(screen.getByRole('alertdialog')).toHaveFocus()
  })

  it('renders resolved statuses without buttons', () => {
    render(<ApprovalCard approval={pendingCard({status: 'timeout'})} onRespond={vi.fn()} />)
    expect(screen.queryByText('Approve & apply')).not.toBeInTheDocument()
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('No response in time')
  })

  // The stream ended before a decision (the server is restarting): the card
  // says so with the server's words, rather than reading as a failed fix.
  it('shows a cancelled card with the server message and no buttons', () => {
    render(
      <ApprovalCard
        approval={pendingCard({status: 'cancelled', message: 'the server is restarting'})}
        onRespond={vi.fn()}
      />,
    )
    expect(screen.queryByText('Approve & apply')).not.toBeInTheDocument()
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent(
      'Cancelled — nothing was changed. the server is restarting',
    )
  })

  it('reports decisions with the card proposalId', () => {
    const onRespond = vi.fn()
    render(<ApprovalCard approval={batchCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Approve & apply 2 fixes'))
    expect(onRespond).toHaveBeenCalledWith(true, 'batch-1', [])
  })

  describe('batch', () => {
    it('renders one card with a per-fix row list', () => {
      render(<ApprovalCard approval={batchCard()} onRespond={vi.fn()} />)
      expect(screen.getByText('Proposed 2 fixes — approval needed')).toBeInTheDocument()
      expect(screen.getByText('Approve & apply 2 fixes')).toBeInTheDocument()
      const items = screen.getAllByTestId('fix-proposal-items')
      expect(items).toHaveLength(1)
      expect(screen.getByText('Sync API')).toBeInTheDocument()
      expect(screen.getByText('wrap-in-retry')).toBeInTheDocument()
    })

    it('renders per-item outcomes after the batch decision resolves', () => {
      render(
        <ApprovalCard
          approval={batchCard({
            status: 'applied-unresolved',
            message: 'review needed',
            items: [
              fixItem({summary: 'wrap', status: 'applied'}),
              fixItem({
                id: 'missing-retry',
                label: 'Sync API',
                ruleId: 'missing-retry',
                fixType: 'wrap-in-retry',
                blockLabel: 'Sync API',
                line: 8,
                summary: 'retry',
                status: 'applied-unresolved',
                message: 'still appears',
              }),
            ],
          })}
          onRespond={vi.fn()}
        />,
      )
      const statuses = screen.getAllByTestId('fix-item-status')
      expect(statuses[0]).toHaveTextContent('applied')
      expect(statuses[1]).toHaveTextContent('applied — still appears')
      expect(statuses[1]).toHaveTextContent('still appears')
      expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('review recommended')
    })

    it('Escape dismisses a pending batch with its proposalId', () => {
      const onRespond = vi.fn()
      render(<ApprovalCard approval={batchCard()} onRespond={onRespond} />)
      fireEvent.keyDown(screen.getByRole('alertdialog'), {key: 'Escape'})
      expect(onRespond).toHaveBeenCalledWith(false, 'batch-1')
    })
  })
})

describe('batch per-item opt-out (U4.1)', () => {
  it('deselecting an item excludes its index from the approval', () => {
    const onRespond = vi.fn()
    const batch = batchCard()
    render(<ApprovalCard approval={batch} onRespond={onRespond} />)

    // Deselect the second item (index 1).
    fireEvent.click(screen.getByTestId('include-item-1'))
    fireEvent.click(screen.getByRole('button', {name: /approve & apply 1/i}))
    expect(onRespond).toHaveBeenCalledWith(true, batch.proposalId, [1])
  })

  it('deselecting everything disables Approve', () => {
    const onRespond = vi.fn()
    const batch = batchCard()
    render(<ApprovalCard approval={batch} onRespond={onRespond} />)
    fireEvent.click(screen.getByTestId('include-item-0'))
    fireEvent.click(screen.getByTestId('include-item-1'))
    const approve = screen.getByRole('button', {name: /approve & apply 0/i}) as HTMLButtonElement
    expect(approve.disabled).toBe(true)
    fireEvent.click(approve)
    expect(onRespond).not.toHaveBeenCalled()
  })
})

// A preview kind this bundle does not know still gets a decision: the tool's
// name, each item's label and summary as text, Approve/Dismiss and the
// countdown — never a silent 60 s timeout.
describe('ApprovalCard (unknown preview kind)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('an unknown previewKind renders the generic preview and still approves/dismisses', () => {
    const onRespond = vi.fn()
    const {rerender} = render(<ApprovalCard approval={genericCard()} onRespond={onRespond} />)

    expect(screen.getByRole('alertdialog', {name: 'rename_variable: approval needed'})).toBeInTheDocument()
    expect(screen.getByText('rename_variable')).toBeInTheDocument()
    expect(screen.getByText('Variable %Customer%')).toBeInTheDocument()
    expect(screen.getByText('Rename to %CustomerRecord%')).toBeInTheDocument()
    expect(screen.getByText(/requires write access/i)).toBeInTheDocument()
    expect(screen.getByText(/60s to decide/)).toBeInTheDocument()
    // Nothing of the fix vocabulary leaks in.
    expect(screen.queryByText(/rule/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Proposed fix/)).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Approve'}))
    expect(onRespond).toHaveBeenCalledWith(true, 'a1', undefined)
    rerender(<ApprovalCard approval={genericCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByRole('button', {name: 'Dismiss'}))
    expect(onRespond).toHaveBeenCalledWith(false, 'a1')
  })

  it('lets a batch of unknown-kind items be approved in part, and shows their outcomes', () => {
    const onRespond = vi.fn()
    const batch = genericCard({
      items: [
        {id: 'v1', label: 'Variable %A%', summary: 'to %Alpha%', status: 'pending'},
        {id: 'v2', label: 'Variable %B%', summary: 'to %Beta%', status: 'pending'},
      ],
    })
    const {rerender} = render(<ApprovalCard approval={batch} onRespond={onRespond} />)
    fireEvent.click(screen.getByTestId('include-item-1'))
    fireEvent.click(screen.getByRole('button', {name: 'Approve 1 item'}))
    expect(onRespond).toHaveBeenCalledWith(true, 'a1', [1])

    rerender(
      <ApprovalCard
        approval={genericCard({
          status: 'applied',
          items: [
            {id: 'v1', label: 'Variable %A%', summary: 'to %Alpha%', status: 'applied'},
            {id: 'v2', label: 'Variable %B%', summary: 'to %Beta%', status: 'error', message: 'skipped'},
          ],
        })}
        onRespond={onRespond}
      />,
    )
    expect(screen.queryByRole('button', {name: /approve/i})).not.toBeInTheDocument()
    const statuses = screen.getAllByTestId('fix-item-status')
    expect(statuses[0]).toHaveTextContent('done')
    expect(statuses[1]).toHaveTextContent('failed — skipped')
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('Done.')
  })

  it('Escape dismisses a pending generic request', () => {
    const onRespond = vi.fn()
    render(<ApprovalCard approval={genericCard()} onRespond={onRespond} />)
    fireEvent.keyDown(screen.getByRole('alertdialog'), {key: 'Escape'})
    expect(onRespond).toHaveBeenCalledWith(false, 'a1')
  })

  // F2: the kind is a server string used as a key; a prototype member's name
  // must not resolve to Object.prototype (and render a function as a child).
  it('a previewKind naming a prototype key renders the generic preview', () => {
    render(<ApprovalCard approval={genericCard({previewKind: 'constructor'})} onRespond={vi.fn()} />)
    const card = screen.getByRole('alertdialog', {name: 'rename_variable: approval needed'})
    expect(card).toHaveAttribute('data-preview-kind', 'constructor')
    expect(screen.getByText('Rename to %CustomerRecord%')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Approve'})).toBeInTheDocument()
  })

  // F3: the summary is the server's text, unbounded in length; the fix
  // preview scrolls past a height, and so must this one.
  it('bounds the summary height like the fix preview', () => {
    render(<ApprovalCard approval={genericCard()} onRespond={vi.fn()} />)
    expect(screen.getByText('Rename to %CustomerRecord%')).toHaveClass('max-h-40', 'overflow-auto')
  })
})
