import type {Block, BlockType, FlowDocument, Subflow, Highlight} from '@/types'

export interface FlatBlock {
  block: Block
  depth: number
  isLast: boolean
  isContainer: boolean
  collapsed: boolean
}

const CONTAINER_TYPES = new Set(['LOOP', 'CONDITION', 'ERROR_HANDLER', 'BLOCK', 'SWITCH', 'ELSE', 'CASE', 'DEFAULT'])

const STRUCTURAL_TYPES = new Set(['END', 'ELSE', 'CASE', 'DEFAULT'])

export function flattenBlocks(blocks: Block[], expandedBlockIds: Set<string>, depth = 0): FlatBlock[] {
  const result: FlatBlock[] = []
  appendFlatBlocks(blocks, expandedBlockIds, depth, result)
  return result
}

function appendFlatBlocks(blocks: Block[], expandedBlockIds: Set<string>, depth: number, out: FlatBlock[]): void {
  const last = blocks.length - 1
  for (let i = 0; i <= last; i++) {
    const block = blocks[i]
    if (!block) continue
    const isContainer = CONTAINER_TYPES.has(block.type)
    const collapsed = expandedBlockIds.has(block.id)
    const isStructural = STRUCTURAL_TYPES.has(block.type)

    out.push({
      block,
      depth: isStructural ? Math.max(0, depth - 1) : depth,
      isLast: i === last,
      isContainer,
      collapsed,
    })

    if (isContainer && !collapsed && block.children.length > 0) {
      const depthIncrease = block.type === 'ELSE' || block.type === 'CASE' || block.type === 'DEFAULT' ? 0 : 1
      appendFlatBlocks(block.children, expandedBlockIds, depth + depthIncrease, out)
    }
  }
}

export type TreeRow = {
  kind: 'subflow' | 'block'
  id: string
  depth: number
  name: string
  blockType?: BlockType
  childCount: number
  hasChildren: boolean
  subflowId: string
  blockData?: Block
  subflowData?: Subflow
  highlights?: Highlight[]
}

export interface FlattenTreeOptions {
  expandedSubflowIds: Set<string>
  expandedBlockIds: Set<string>
  visibleTypes: Set<BlockType>
  searchQuery?: string
  matchedBlockIds?: Set<string>
}

function blockRow(block: Block, depth: number, subflowId: string): TreeRow {
  return {
    kind: 'block',
    id: block.id,
    depth,
    name: block.name,
    blockType: block.type,
    childCount: block.children.length,
    hasChildren: block.children.length > 0,
    subflowId,
    blockData: block,
  }
}

function appendTreeBlockRows(
  blocks: Block[],
  subflowId: string,
  depth: number,
  expandedBlockIds: Set<string>,
  visibleTypes: Set<BlockType>,
  qLower: string | undefined,
  matchedBlockIds: Set<string> | undefined,
  out: TreeRow[],
): void {
  const hasSearch = !!(qLower || matchedBlockIds?.size)

  for (const block of blocks) {
    if (!visibleTypes.has(block.type)) continue

    if (!hasSearch) {
      out.push(blockRow(block, depth, subflowId))
      if (expandedBlockIds.has(block.id) && block.children.length > 0) {
        appendTreeBlockRows(
          block.children,
          subflowId,
          depth + 1,
          expandedBlockIds,
          visibleTypes,
          qLower,
          matchedBlockIds,
          out,
        )
      }
      continue
    }

    const nameMatch = !qLower || block.name.toLowerCase().includes(qLower)
    const isMatched = matchedBlockIds?.has(block.id)
    const childRows: TreeRow[] = []
    appendTreeBlockRows(
      block.children,
      subflowId,
      depth + 1,
      expandedBlockIds,
      visibleTypes,
      qLower,
      matchedBlockIds,
      childRows,
    )
    const hasMatchingChildren = childRows.length > 0

    if (!nameMatch && !isMatched && !hasMatchingChildren) continue

    out.push(blockRow(block, depth, subflowId))

    if (expandedBlockIds.has(block.id) || hasMatchingChildren) {
      for (const row of childRows) out.push(row)
    }
  }
}

export function flattenTreeRows(doc: FlowDocument, options: FlattenTreeOptions): TreeRow[] {
  const {expandedSubflowIds, expandedBlockIds, visibleTypes, searchQuery, matchedBlockIds} = options
  const rows: TreeRow[] = []
  const qLower = searchQuery?.toLowerCase()
  const hasSearch = !!(qLower || matchedBlockIds?.size)

  for (const subflow of doc.subflows) {
    const subflowRow: TreeRow = {
      kind: 'subflow',
      id: subflow.id,
      depth: 0,
      name: subflow.name,
      childCount: subflow.blocks.length,
      hasChildren: subflow.blocks.length > 0,
      subflowId: subflow.id,
      subflowData: subflow,
    }

    if (!hasSearch) {
      rows.push(subflowRow)
      if (expandedSubflowIds.has(subflow.id)) {
        appendTreeBlockRows(
          subflow.blocks,
          subflow.id,
          1,
          expandedBlockIds,
          visibleTypes,
          qLower,
          matchedBlockIds,
          rows,
        )
      }
      continue
    }

    const sfMatch = !qLower || subflow.name.toLowerCase().includes(qLower)
    const blockRows: TreeRow[] = []
    appendTreeBlockRows(
      subflow.blocks,
      subflow.id,
      1,
      expandedBlockIds,
      visibleTypes,
      qLower,
      matchedBlockIds,
      blockRows,
    )
    const hasMatchingChildren = blockRows.length > 0
    if (!sfMatch && !hasMatchingChildren) continue

    rows.push(subflowRow)
    if (expandedSubflowIds.has(subflow.id) || hasMatchingChildren) {
      for (const row of blockRows) rows.push(row)
    }
  }
  return rows
}

function findBlockInTree(blocks: Block[], id: string): Block | null {
  for (const block of blocks) {
    if (block.id === id) return block
    if (block.children.length > 0) {
      const found = findBlockInTree(block.children, id)
      if (found) return found
    }
  }
  return null
}

export function findBlockInDoc(
  doc: FlowDocument,
  blockId: string,
): {block: Block; subflowId: string; subflowName: string} | null {
  for (const sf of doc.subflows) {
    const found = findBlockInTree(sf.blocks, blockId)
    if (found) return {block: found, subflowId: sf.id, subflowName: sf.name}
  }
  return null
}

export type BlockLookup = Map<string, {name: string; subflowName: string; rawType: string}>

export function buildBlockLookup(doc: FlowDocument): BlockLookup {
  const map: BlockLookup = new Map()
  const walk = (blocks: Block[], subflowName: string) => {
    for (const b of blocks) {
      map.set(b.id, {name: b.name, subflowName, rawType: b.rawType})
      if (b.children.length > 0) walk(b.children, subflowName)
    }
  }
  for (const sf of doc.subflows) walk(sf.blocks, sf.name)
  return map
}

export function findSubflowIdByBlock(doc: FlowDocument, blockId: string): string | null {
  for (const sf of doc.subflows) {
    if (findBlockInTree(sf.blocks, blockId)) return sf.id
  }
  return null
}

export function findBlockPath(blocks: Block[], targetId: string): Block[] | null {
  for (const block of blocks) {
    if (block.id === targetId) return [block]
    if (block.children.length > 0) {
      const subPath = findBlockPath(block.children, targetId)
      if (subPath) return [block, ...subPath]
    }
  }
  return null
}

export function findAncestorIds(doc: FlowDocument, blockId: string): string[] {
  for (const sf of doc.subflows) {
    const path = findBlockPath(sf.blocks, blockId)
    if (path) return path.slice(0, -1).map(b => b.id)
  }
  return []
}

export function findLabelBlock(doc: FlowDocument, labelName: string): Block | null {
  for (const sf of doc.subflows) {
    const found = findLabelInTree(sf.blocks, labelName)
    if (found) return found
  }
  return null
}

function findLabelInTree(blocks: Block[], labelName: string): Block | null {
  for (const block of blocks) {
    if (block.rawType === 'LABEL' && block.name === labelName) return block
    if (block.children.length > 0) {
      const found = findLabelInTree(block.children, labelName)
      if (found) return found
    }
  }
  return null
}
