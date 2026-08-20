import {describe, it, expect} from 'vitest'
import {flattenBlocks, flattenTreeRows, buildBlockLookup} from './tree'
import type {Block, FlowDocument, Subflow} from '@/types'

// ---- helpers ----

function makeBlock(id: string, type: Block['type'] = 'ACTION', children: Block[] = []): Block {
  return {
    id,
    name: `Block ${id}`,
    type,
    rawType: type,
    indent: 0,
    lineNumber: 0,
    children,
    properties: {},
    variables: [],
    subflowId: 'sf1',
  }
}

function makeSubflow(id: string, blocks: Block[] = []): Subflow {
  return {id, name: `Subflow ${id}`, blocks, variables: []}
}

function makeDoc(...subflows: Subflow[]): FlowDocument {
  return {
    id: 'doc1',
    name: 'Test',
    subflows,
    variables: [],
    findingsCount: 0,
    flows: [],
  } as unknown as FlowDocument
}

const allTypes = new Set<Block['type']>([
  'ACTION',
  'LOOP',
  'CONDITION',
  'SUBFLOW',
  'ERROR_HANDLER',
  'COMMENT',
  'VARIABLE',
  'WAIT',
  'BLOCK',
  'SWITCH',
  'ELSE',
  'CASE',
  'DEFAULT',
  'END',
  'UNKNOWN',
])

// ---- flattenBlocks ----

describe('flattenBlocks', () => {
  it('returns empty for empty input', () => {
    expect(flattenBlocks([], new Set())).toEqual([])
  })

  it('flattens a flat list at depth 0', () => {
    const blocks = [makeBlock('a'), makeBlock('b'), makeBlock('c')]
    const result = flattenBlocks(blocks, new Set())
    expect(result).toHaveLength(3)
    expect(result.map(r => r.block.id)).toEqual(['a', 'b', 'c'])
    expect(result.every(r => r.depth === 0)).toBe(true)
    expect(result[2].isLast).toBe(true)
    expect(result[0].isLast).toBe(false)
  })

  it('marks container blocks correctly', () => {
    const loop = makeBlock('loop1', 'LOOP')
    const result = flattenBlocks([loop], new Set())
    expect(result[0].isContainer).toBe(true)
  })

  it('marks non-container blocks correctly', () => {
    const action = makeBlock('a1', 'ACTION')
    const result = flattenBlocks([action], new Set())
    expect(result[0].isContainer).toBe(false)
  })

  it('hides children when block id is in expandedBlockIds (collapsed)', () => {
    const child = makeBlock('child')
    const loop = makeBlock('loop1', 'LOOP', [child])
    // "expandedBlockIds" is inverted naming: having the id means collapsed
    const result = flattenBlocks([loop], new Set(['loop1']))
    expect(result).toHaveLength(1)
    expect(result[0].collapsed).toBe(true)
  })

  it('shows children when block id is NOT in expandedBlockIds', () => {
    const child = makeBlock('child')
    const loop = makeBlock('loop1', 'LOOP', [child])
    const result = flattenBlocks([loop], new Set())
    expect(result).toHaveLength(2)
    expect(result[1].block.id).toBe('child')
    expect(result[1].depth).toBe(1)
  })

  it('indents nested children correctly', () => {
    const grandchild = makeBlock('gc')
    const child = makeBlock('child', 'LOOP', [grandchild])
    const parent = makeBlock('parent', 'LOOP', [child])
    const result = flattenBlocks([parent], new Set())
    expect(result.map(r => r.depth)).toEqual([0, 1, 2])
  })

  it('structural types (ELSE, CASE, DEFAULT) get depth - 1', () => {
    const elseBranch = makeBlock('else1', 'ELSE')
    const cond = makeBlock('cond1', 'CONDITION', [elseBranch])
    const result = flattenBlocks([cond], new Set())
    // ELSE at depth 1 should render at depth 0
    const elseRow = result.find(r => r.block.id === 'else1')!
    expect(elseRow.depth).toBe(0)
  })

  it('ELSE/CASE/DEFAULT do not add extra nesting for their children', () => {
    const action = makeBlock('act1')
    const elseBranch = makeBlock('else1', 'ELSE', [action])
    const cond = makeBlock('cond1', 'CONDITION', [elseBranch])
    const result = flattenBlocks([cond], new Set())
    const actRow = result.find(r => r.block.id === 'act1')!
    // ELSE is at depth 0 (structural), its child should be at depth 1 not depth 2
    expect(actRow.depth).toBe(1)
  })
})

// ---- flattenTreeRows ----

describe('flattenTreeRows', () => {
  const baseOptions = {
    expandedSubflowIds: new Set<string>(),
    expandedBlockIds: new Set<string>(),
    visibleTypes: allTypes,
  }

  it('returns empty for doc with no subflows', () => {
    const doc = makeDoc()
    expect(flattenTreeRows(doc, baseOptions)).toEqual([])
  })

  it('returns only subflow header when subflow is collapsed', () => {
    const sf = makeSubflow('sf1', [makeBlock('b1')])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, baseOptions)
    expect(result).toHaveLength(1)
    expect(result[0].kind).toBe('subflow')
    expect(result[0].id).toBe('sf1')
  })

  it('returns subflow + blocks when subflow is expanded', () => {
    const sf = makeSubflow('sf1', [makeBlock('b1'), makeBlock('b2')])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1']),
    })
    expect(result).toHaveLength(3)
    expect(result[0].kind).toBe('subflow')
    expect(result[1].kind).toBe('block')
    expect(result[2].kind).toBe('block')
  })

  it('block rows have depth 1', () => {
    const sf = makeSubflow('sf1', [makeBlock('b1')])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1']),
    })
    expect(result[1].depth).toBe(1)
  })

  it('filters out blocks not in visibleTypes', () => {
    const sf = makeSubflow('sf1', [makeBlock('a1', 'ACTION'), makeBlock('c1', 'COMMENT')])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1']),
      visibleTypes: new Set<Block['type']>(['ACTION']),
    })
    expect(result).toHaveLength(2) // subflow header + 1 visible block
    expect(result[1].blockType).toBe('ACTION')
  })

  it('search: hides non-matching subflows', () => {
    const sf1 = makeSubflow('sf1', [])
    const sf2 = makeSubflow('sf2', [])
    // Override names for search
    sf1.name = 'Alpha'
    sf2.name = 'Beta'
    const doc = makeDoc(sf1, sf2)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      searchQuery: 'alpha',
    })
    expect(result).toHaveLength(1)
    expect(result[0].id).toBe('sf1')
  })

  it('search: shows subflow when a child block matches', () => {
    const block = makeBlock('b1', 'ACTION')
    block.name = 'Set Variable'
    const sf = makeSubflow('sf1', [block])
    sf.name = 'NoMatch'
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      searchQuery: 'variable',
    })
    // subflow should be included because child matches
    expect(result.some(r => r.id === 'sf1')).toBe(true)
    expect(result.some(r => r.id === 'b1')).toBe(true)
  })

  it('search: shows only matched blocks when query + matchedBlockIds are both set', () => {
    const b1 = makeBlock('b1')
    b1.name = 'Get Variable'
    const b2 = makeBlock('b2')
    b2.name = 'Send Email'
    const sf = makeSubflow('sf1', [b1, b2])
    const doc = makeDoc(sf)
    // Query matches b1 by name; matchedBlockIds also points to b1 from search results
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      searchQuery: 'get variable',
      matchedBlockIds: new Set(['b1']),
    })
    expect(result.some(r => r.id === 'b1')).toBe(true)
    expect(result.some(r => r.id === 'b2')).toBe(false)
  })

  it('matchedBlockIds shows a block even when its name does not match the query', () => {
    const b1 = makeBlock('b1')
    b1.name = 'Unrelated Name'
    const sf = makeSubflow('sf1', [b1])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      searchQuery: 'something',
      matchedBlockIds: new Set(['b1']),
    })
    expect(result.some(r => r.id === 'b1')).toBe(true)
  })

  it('multiple subflows are all included when expanded', () => {
    const sf1 = makeSubflow('sf1', [makeBlock('b1')])
    const sf2 = makeSubflow('sf2', [makeBlock('b2')])
    const doc = makeDoc(sf1, sf2)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1', 'sf2']),
    })
    expect(result).toHaveLength(4) // 2 headers + 2 blocks
  })

  it('nested blocks are not expanded when block id is absent from expandedBlockIds', () => {
    const child = makeBlock('child')
    const loop = {...makeBlock('loop1', 'LOOP', [child]), children: [child]}
    const sf = makeSubflow('sf1', [loop])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1']),
    })
    expect(result).toHaveLength(2) // header + loop only
    expect(result.every(r => r.id !== 'child')).toBe(true)
  })

  it('nested blocks are shown when block id is in expandedBlockIds', () => {
    const child = makeBlock('child')
    const loop = makeBlock('loop1', 'LOOP', [child])
    const sf = makeSubflow('sf1', [loop])
    const doc = makeDoc(sf)
    const result = flattenTreeRows(doc, {
      ...baseOptions,
      expandedSubflowIds: new Set(['sf1']),
      expandedBlockIds: new Set(['loop1']),
    })
    expect(result).toHaveLength(3) // header + loop + child
    expect(result[2].id).toBe('child')
    expect(result[2].depth).toBe(2)
  })
})

// ---- buildBlockLookup ----

describe('buildBlockLookup', () => {
  it('indexes every block (including nested) with its name and subflow name', () => {
    const child = makeBlock('child')
    const loop = makeBlock('loop1', 'LOOP', [child])
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('a'), loop]))
    const lookup = buildBlockLookup(doc)
    expect(lookup.size).toBe(3)
    expect(lookup.get('a')).toEqual({name: 'Block a', subflowName: 'Subflow sf1', rawType: 'ACTION'})
    expect(lookup.get('loop1')).toEqual({name: 'Block loop1', subflowName: 'Subflow sf1', rawType: 'LOOP'})
    expect(lookup.get('child')).toEqual({name: 'Block child', subflowName: 'Subflow sf1', rawType: 'ACTION'})
  })

  it('attributes blocks to their containing subflow', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('a')]), makeSubflow('sf2', [makeBlock('b')]))
    const lookup = buildBlockLookup(doc)
    expect(lookup.get('a')?.subflowName).toBe('Subflow sf1')
    expect(lookup.get('b')?.subflowName).toBe('Subflow sf2')
  })

  it('returns an empty map for a doc with no blocks', () => {
    expect(buildBlockLookup(makeDoc()).size).toBe(0)
  })
})
