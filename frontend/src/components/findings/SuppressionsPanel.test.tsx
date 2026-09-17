import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'

const listSuppressions = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {listSuppressions: (...a: unknown[]) => listSuppressions(...a)},
}))

import SuppressionsPanel from './SuppressionsPanel'

describe('SuppressionsPanel', () => {
  beforeEach(() => {
    listSuppressions.mockReset()
  })

  // The audit re-runs every rule, so nothing is fetched until the user opens it.
  it('fetches only when opened, then lists entries with a stale marker', async () => {
    listSuppressions.mockResolvedValue([
      {line: 3, rule: 'missing-timeout', blockLabel: 'Invoke web service', stale: false},
      {line: 7, rule: 'missing-timeout', blockLabel: 'Wait', stale: true, reason: 'rule does not currently fire here'},
    ])
    render(<SuppressionsPanel flowId="flow-1" reportVersion="r1" />)
    expect(listSuppressions).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', {name: /Inline suppressions/}))

    expect(await screen.findByText('Invoke web service')).toBeInTheDocument()
    expect(listSuppressions).toHaveBeenCalledWith('flow-1')
    const stale = screen.getAllByText('Stale')
    expect(stale).toHaveLength(1)
    expect(stale[0]).toHaveAttribute('title', 'rule does not currently fire here')
    expect(screen.getByText('1 stale')).toBeInTheDocument()
  })

  it('says so when the audit fails instead of showing an empty list', async () => {
    listSuppressions.mockRejectedValue(new Error('boom'))
    render(<SuppressionsPanel flowId="flow-1" />)
    fireEvent.click(screen.getByRole('button', {name: /Inline suppressions/}))
    expect(await screen.findByText("Couldn't load inline suppressions.")).toBeInTheDocument()
  })
})
