import {describe, it, expect, beforeEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useFlattenedBlocks} from './useFlattenedBlocks'
import {useFlowStore} from '@/stores/flowStore'
import type {Block, FlowDocument, Subflow} from '@/types'

function makeBlock(id: string, subflowId = 'sf1', children: Block[] = []): Block {
  return {
    id,
    name: `Block ${id}`,
    type: 'ACTION',
    rawType: 'ACTION',
    indent: 0,
    lineNumber: 0,
    children,
    properties: {},
    variables: [],
    subflowId,
  }
}

function makeSubflow(id: string, blocks: Block[] = []): Subflow {
  return {id, name: `Subflow ${id}`, blocks, variables: []}
}

function makeDoc(...subflows: Subflow[]): FlowDocument {
  return {id: 'doc1', name: 'Test', subflows, variables: [], findingsCount: 0, flows: []} as unknown as FlowDocument
}

beforeEach(() => {
  useFlowStore.getState().reset()
})

describe('useFlattenedBlocks', () => {
  it('returns an empty array when there is no document', () => {
    const {result} = renderHook(() => useFlattenedBlocks())
    expect(result.current).toEqual([])
  })

  it('flattens the selected subflow (falling back to selectedSubflowId, then the first subflow)', () => {
    const sf1 = makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')])
    const sf2 = makeSubflow('sf2', [makeBlock('b3', 'sf2')])
    useFlowStore.setState({document: makeDoc(sf1, sf2), selectedSubflowId: 'sf2'})

    const {result} = renderHook(() => useFlattenedBlocks())
    expect(result.current.map(fb => fb.block.id)).toEqual(['b3'])
  })

  it('uses the first subflow when selectedSubflowId does not match any subflow', () => {
    const sf1 = makeSubflow('sf1', [makeBlock('b1', 'sf1')])
    useFlowStore.setState({document: makeDoc(sf1), selectedSubflowId: 'missing'})

    const {result} = renderHook(() => useFlattenedBlocks())
    expect(result.current.map(fb => fb.block.id)).toEqual(['b1'])
  })

  it('an explicit subflowId argument overrides the store selection', () => {
    const sf1 = makeSubflow('sf1', [makeBlock('b1', 'sf1')])
    const sf2 = makeSubflow('sf2', [makeBlock('b2', 'sf2')])
    useFlowStore.setState({document: makeDoc(sf1, sf2), selectedSubflowId: 'sf1'})

    const {result} = renderHook(() => useFlattenedBlocks('sf2'))
    expect(result.current.map(fb => fb.block.id)).toEqual(['b2'])
  })

  it('marks isLast on the final block at each level', () => {
    const sf1 = makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')])
    useFlowStore.setState({document: makeDoc(sf1), selectedSubflowId: 'sf1'})

    const {result} = renderHook(() => useFlattenedBlocks())
    expect(result.current.find(fb => fb.block.id === 'b1')?.isLast).toBe(false)
    expect(result.current.find(fb => fb.block.id === 'b2')?.isLast).toBe(true)
  })
})
