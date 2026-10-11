import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import DetailsTab from './DetailsTab'
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument, Block} from '@/types'

function makeBlock(id: string, name: string): Block {
  return {
    id,
    name,
    type: 'ACTION',
    rawType: 'Test.Action',
    properties: {},
    variables: [],
    children: [],
    subflowId: 'sf1',
    indent: 0,
    lineNumber: 0,
  }
}

function makeDoc(): FlowDocument {
  return {
    id: 'flow1',
    name: 'My Flow',
    filePath: '/flow1.txt',
    subflows: [{id: 'sf1', name: 'Main', blocks: [makeBlock('b1', 'Step One')], variables: []}],
    metadata: {blockCount: 1, subflowCount: 1, maxDepth: 1, parsedAt: '', fileSize: 0, rawLineCount: 0},
  }
}

const initialState = useFlowStore.getState()

describe('DetailsTab', () => {
  beforeEach(() => {
    useFlowStore.setState(initialState, true)
  })

  it('renders the placeholder when nothing is selected', () => {
    render(<DetailsTab />)
    expect(screen.getByText('Select a block')).toBeTruthy()
  })

  it('renders block details after a block is selected on an already-mounted instance', () => {
    render(<DetailsTab />)
    expect(screen.getByText('Select a block')).toBeTruthy()

    act(() => {
      useFlowStore.setState({document: makeDoc(), selectedBlockId: 'b1', selectedSubflowId: 'sf1'})
    })

    expect(screen.queryByText('Select a block')).toBeNull()
    expect(screen.getByText('Step One')).toBeTruthy()
  })

  it('renders nothing (null) when the selected block id has no matching block', () => {
    const {container} = render(<DetailsTab />)
    act(() => {
      useFlowStore.setState({document: makeDoc(), selectedBlockId: 'missing', selectedSubflowId: 'sf1'})
    })
    expect(container.firstChild).toBeNull()
  })
})
