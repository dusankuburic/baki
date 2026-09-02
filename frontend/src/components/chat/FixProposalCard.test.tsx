import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import FixProposalCard from './FixProposalCard'
import type {FixProposalCard as CardState} from '@/stores/chatStore'

function pendingCard(overrides: Partial<CardState> = {}): CardState {
  return {
    proposalId: 'p1',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'pending',
    items: [
      {
        ruleId: 'unhandled-error',
        fixType: 'wrap-error-handler',
        blockLabel: 'Call API',
        line: 3,
        summary: 'wrap lines 3-3',
        status: 'pending',
      },
    ],
    ...overrides,
  }
}

function batchCard(overrides: Partial<CardState> = {}): CardState {
  return {
    proposalId: 'batch-1',
    status: 'pending',
    ruleId: '',
    fixType: '',
    blockLabel: '',
    line: 0,
    summary: '',
    items: [
      {ruleId: 'unhandled-error', fixType: 'wrap-error-handler', blockLabel: 'Call API', line: 3, summary: 'wrap', status: 'pending'},
      {ruleId: 'missing-retry', fixType: 'wrap-in-retry', blockLabel: 'Sync API', line: 8, summary: 'retry', status: 'pending'},
    ],
    ...overrides,
  }
}

describe('FixProposalCard', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders the pending approval prompt with buttons and countdown', () => {
    const onRespond = vi.fn()
    render(<FixProposalCard proposal={pendingCard()} onRespond={onRespond} />)

    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    expect(screen.getByText('Approve & apply')).toBeInTheDocument()
    expect(screen.getByText('Dismiss')).toBeInTheDocument()
    expect(screen.getByText(/60s to decide/)).toBeInTheDocument()
  })

  it('counts down the decision window while pending', () => {
    render(<FixProposalCard proposal={pendingCard()} onRespond={vi.fn()} />)
    act(() => {
      vi.advanceTimersByTime(15_000)
    })
    expect(screen.getByText(/45s to decide/)).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(50_000)
    })
    expect(screen.getByText(/expiring…/)).toBeInTheDocument()
  })

  it('stops the countdown once resolved', () => {
    const {rerender} = render(<FixProposalCard proposal={pendingCard()} onRespond={vi.fn()} />)
    act(() => {
      vi.advanceTimersByTime(10_000)
    })
    rerender(<FixProposalCard proposal={pendingCard({status: 'applied', message: 'verified'})} onRespond={vi.fn()} />)
    expect(screen.queryByText(/to decide/)).not.toBeInTheDocument()
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('Fix applied and verified')
  })

  it('reports Approve and Dismiss decisions', () => {
    const onRespond = vi.fn()
    const {rerender} = render(<FixProposalCard proposal={pendingCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Approve & apply'))
    expect(onRespond).toHaveBeenCalledWith(true, 'p1', undefined)
    rerender(<FixProposalCard proposal={pendingCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Dismiss'))
    expect(onRespond).toHaveBeenCalledWith(false, 'p1')
  })

  it('Escape dismisses while pending', () => {
    const onRespond = vi.fn()
    render(<FixProposalCard proposal={pendingCard()} onRespond={onRespond} />)
    fireEvent.keyDown(screen.getByRole('alertdialog'), {key: 'Escape'})
    expect(onRespond).toHaveBeenCalledWith(false, 'p1')
  })

  it('moves focus to the card when the prompt appears', () => {
    render(<FixProposalCard proposal={pendingCard()} onRespond={vi.fn()} />)
    expect(screen.getByRole('alertdialog')).toHaveFocus()
  })

  it('renders resolved statuses without buttons', () => {
    render(<FixProposalCard proposal={pendingCard({status: 'timeout'})} onRespond={vi.fn()} />)
    expect(screen.queryByText('Approve & apply')).not.toBeInTheDocument()
    expect(screen.getByTestId('fix-proposal-status')).toHaveTextContent('No response in time')
  })

  it('reports decisions with the card proposalId', () => {
    const onRespond = vi.fn()
    render(<FixProposalCard proposal={batchCard()} onRespond={onRespond} />)
    fireEvent.click(screen.getByText('Approve & apply 2 fixes'))
    expect(onRespond).toHaveBeenCalledWith(true, 'batch-1', [])
  })

  describe('batch', () => {
    it('renders one card with a per-fix row list', () => {
      render(<FixProposalCard proposal={batchCard()} onRespond={vi.fn()} />)
      expect(screen.getByText('Proposed 2 fixes — approval needed')).toBeInTheDocument()
      expect(screen.getByText('Approve & apply 2 fixes')).toBeInTheDocument()
      const items = screen.getAllByTestId('fix-proposal-items')
      expect(items).toHaveLength(1)
      expect(screen.getByText('Sync API')).toBeInTheDocument()
      expect(screen.getByText('wrap-in-retry')).toBeInTheDocument()
    })

    it('renders per-item outcomes after the batch decision resolves', () => {
      render(
        <FixProposalCard
          proposal={batchCard({
            status: 'applied-unresolved',
            message: 'review needed',
            items: [
              {ruleId: 'unhandled-error', fixType: 'wrap-error-handler', blockLabel: 'Call API', line: 3, summary: 'wrap', status: 'applied'},
              {ruleId: 'missing-retry', fixType: 'wrap-in-retry', blockLabel: 'Sync API', line: 8, summary: 'retry', status: 'applied-unresolved', message: 'still appears'},
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
      render(<FixProposalCard proposal={batchCard()} onRespond={onRespond} />)
      fireEvent.keyDown(screen.getByRole('alertdialog'), {key: 'Escape'})
      expect(onRespond).toHaveBeenCalledWith(false, 'batch-1')
    })
  })
})

describe('batch per-item opt-out (U4.1)', () => {
  it('deselecting an item excludes its index from the approval', () => {
    const onRespond = vi.fn()
    const batch = batchCard()
    render(<FixProposalCard proposal={batch} onRespond={onRespond} />)

    // Deselect the second item (index 1).
    fireEvent.click(screen.getByTestId('include-item-1'))
    fireEvent.click(screen.getByRole('button', {name: /approve & apply 1/i}))
    expect(onRespond).toHaveBeenCalledWith(true, batch.proposalId, [1])
  })

  it('deselecting everything disables Approve', () => {
    const onRespond = vi.fn()
    const batch = batchCard()
    render(<FixProposalCard proposal={batch} onRespond={onRespond} />)
    fireEvent.click(screen.getByTestId('include-item-0'))
    fireEvent.click(screen.getByTestId('include-item-1'))
    const approve = screen.getByRole('button', {name: /approve & apply 0/i}) as HTMLButtonElement
    expect(approve.disabled).toBe(true)
    fireEvent.click(approve)
    expect(onRespond).not.toHaveBeenCalled()
  })
})
