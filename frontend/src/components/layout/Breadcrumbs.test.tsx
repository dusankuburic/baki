import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Breadcrumbs from './Breadcrumbs'
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument, Block} from '@/types/domain'

function makeBlock(id: string, name: string, children: Block[] = []): Block {
  return {id, name, type: 'ACTION', rawType: 'Test.Action', properties: {}, variables: [], children, subflowId: 'sf1', indent: 0, lineNumber: 0}
}

function makeDoc(): FlowDocument {
  return {
    id: 'flow1',
    name: 'My Flow',
    subflows: [
      {
        id: 'sf1',
        name: 'Main',
        blocks: [
          makeBlock('b1', 'Parent Action', [
            makeBlock('b2', 'Child Action'),
          ]),
        ],
      },
    ],
  } as FlowDocument
}

const initialState = useFlowStore.getState()

describe('Breadcrumbs', () => {
  beforeEach(() => {
    useFlowStore.setState(initialState, true)
  })

  it('renders nothing when no document is loaded', () => {
    useFlowStore.setState({document: null, selectedBlockId: null, selectedSubflowId: null})
    const {container} = render(<Breadcrumbs />)
    expect(container.firstChild).toBeNull()
  })

  it('renders document name and subflow name', () => {
    useFlowStore.setState({
      document: makeDoc(),
      selectedSubflowId: 'sf1',
      selectedBlockId: null,
    })

    render(<Breadcrumbs />)
    expect(screen.getByText('My Flow')).toBeTruthy()
    expect(screen.getByText('Main')).toBeTruthy()
  })

  it('renders block path when a nested block is selected', () => {
    useFlowStore.setState({
      document: makeDoc(),
      selectedSubflowId: 'sf1',
      selectedBlockId: 'b2',
    })

    render(<Breadcrumbs />)
    expect(screen.getByText('Main')).toBeTruthy()
    expect(screen.getByText('Parent Action')).toBeTruthy()
    expect(screen.getByText('Child Action')).toBeTruthy()
  })

  it('calls selectSubflow when clicking a subflow crumb', () => {
    const selectSubflow = vi.fn()
    useFlowStore.setState({
      document: makeDoc(),
      selectedSubflowId: 'sf1',
      selectedBlockId: null,
      selectSubflow,
    })

    render(<Breadcrumbs />)
    fireEvent.click(screen.getByText('My Flow'))
    expect(selectSubflow).toHaveBeenCalled()
  })

  it('calls selectBlock when clicking a block crumb', () => {
    const selectBlock = vi.fn()
    useFlowStore.setState({
      document: makeDoc(),
      selectedSubflowId: 'sf1',
      selectedBlockId: 'b2',
      selectBlock,
      selectSubflow: vi.fn(),
    })

    render(<Breadcrumbs />)
    fireEvent.click(screen.getByText('Parent Action'))
    expect(selectBlock).toHaveBeenCalledWith('b1')
  })
})
