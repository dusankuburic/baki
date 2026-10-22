import {describe, it, expect, vi} from 'vitest'
import {render, screen, waitFor, fireEvent} from '@testing-library/react'
import {installReactFlowEnvironment} from '@/testing/reactFlow'
import RuleDependencyView from './RuleDependencyView'

installReactFlowEnvironment()

const getDependencies = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {getDependencies: (...args: unknown[]) => getDependencies(...args)},
}))

const analysis = {
  topoOrder: ['RULE-A', 'RULE-B', 'RULE-C'],
  dependencies: [
    {fromRuleId: 'RULE-A', toRuleId: 'RULE-B', reason: 'A feeds B'},
    {fromRuleId: 'RULE-B', toRuleId: 'RULE-C', reason: 'B feeds C'},
  ],
  cycles: [['RULE-B', 'RULE-C']],
}

describe('RuleDependencyView', () => {
  it('draws a node per rule, an edge per dependency, and marks the cycle', async () => {
    getDependencies.mockResolvedValue(analysis)
    const {container} = render(<RuleDependencyView />)

    await waitFor(() => expect(container.querySelectorAll('.react-flow__node')).toHaveLength(3))
    expect(container.querySelector('[data-flow-ready]')?.getAttribute('data-flow-ready')).toBe('true')
    expect(screen.getByText('RULE-A')).toBeInTheDocument()

    const cycleNodes = container.querySelectorAll('.flow-node--cycle')
    expect(cycleNodes).toHaveLength(2)
    // Edges need handle positions measured off real layout, which jsdom has none of; the browser
    // side of this is covered by e2e/graph-view.spec.ts.
  })

  it('selects a rule on click and lists what it is related to', async () => {
    getDependencies.mockResolvedValue(analysis)
    const {container} = render(<RuleDependencyView />)
    await waitFor(() => expect(container.querySelectorAll('.react-flow__node').length).toBe(3))

    fireEvent.click(screen.getByText('RULE-A'))

    await waitFor(() => expect(container.querySelector('.flow-node--selected')).not.toBeNull())
    expect(screen.getAllByText('RULE-A').length).toBeGreaterThan(1)
  })

  it('surfaces a failed load', async () => {
    getDependencies.mockRejectedValue(new Error('no backend'))
    render(<RuleDependencyView />)
    expect(await screen.findByText(/no backend/)).toBeInTheDocument()
  })
})
