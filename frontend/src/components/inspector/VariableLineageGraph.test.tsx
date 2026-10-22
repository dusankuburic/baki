import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {installReactFlowEnvironment} from '@/testing/reactFlow'
import VariableLineageGraph from './VariableLineageGraph'
import {useFlowStore} from '@/stores/flowStore'
import type {VariableEvent} from '@/types'

installReactFlowEnvironment()

const event = (blockId: string, line: number, type: VariableEvent['type']): VariableEvent =>
  ({blockId, line, type, subflowId: 'Main', name: 'counter'}) as VariableEvent

describe('VariableLineageGraph', () => {
  beforeEach(() => {
    useFlowStore.setState({navigateToBlock: vi.fn()})
  })

  it('shows one dot per block, laid out and visible', () => {
    const {container} = render(
      <VariableLineageGraph events={[event('b1', 3, 'init'), event('b2', 7, 'mutate'), event('b1', 9, 'read')]} />,
    )

    expect(container.querySelectorAll('.react-flow__node')).toHaveLength(2)
    expect(container.querySelector('[data-flow-ready]')?.getAttribute('data-flow-ready')).toBe('true')
    expect(screen.getByText('L3')).toBeInTheDocument()
    expect(screen.getByText('L7')).toBeInTheDocument()

    // Laid out left to right, so the two dots do not share a position.
    const transforms = [...container.querySelectorAll<HTMLElement>('.react-flow__node')].map(el => el.style.transform)
    expect(new Set(transforms).size).toBe(2)
  })

  it('navigates to the block behind a dot', () => {
    const navigateToBlock = vi.fn()
    useFlowStore.setState({navigateToBlock})
    const {container} = render(<VariableLineageGraph events={[event('b1', 3, 'init'), event('b2', 7, 'read')]} />)

    fireEvent.click(container.querySelector('.react-flow__node')!)
    expect(navigateToBlock).toHaveBeenCalledWith('b1')
  })

  it('says so when there is nothing to draw', () => {
    const {container} = render(<VariableLineageGraph events={[]} />)
    expect(container.querySelector('.react-flow')).toBeNull()
  })
})
