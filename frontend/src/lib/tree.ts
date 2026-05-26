import type {Block, BlockType, FlowDocument, Subflow, Highlight} from '@/types/domain'

// ---- FlatBlock: used by useFlattenedBlocks / block detail view ----

export interface FlatBlock {
    block: Block
    depth: number
    isLast: boolean
    isContainer: boolean
    collapsed: boolean
}

const CONTAINER_TYPES = new Set([
    'LOOP', 'CONDITION', 'ERROR_HANDLER', 'BLOCK', 'SWITCH', 'ELSE', 'CASE', 'DEFAULT',
])

const STRUCTURAL_TYPES = new Set(['END', 'ELSE', 'CASE', 'DEFAULT'])

/**
 * Recursively flattens a block tree into a depth-annotated list.
 * `collapsed` means the block's children are hidden (i.e. its id is in expandedBlockIds —
 * the set name is inverted: "expanded" ids are ones the user has collapsed in the UI).
 */
export function flattenBlocks(
    blocks: Block[],
    expandedBlockIds: Set<string>,
    depth = 0,
): FlatBlock[] {
    const result: FlatBlock[] = []

    blocks.forEach((block, i) => {
        const isContainer = CONTAINER_TYPES.has(block.type)
        const collapsed = expandedBlockIds.has(block.id)
        const isStructural = STRUCTURAL_TYPES.has(block.type)

        result.push({
            block,
            depth: isStructural ? Math.max(0, depth - 1) : depth,
            isLast: i === blocks.length - 1,
            isContainer,
            collapsed,
        })

        if (isContainer && !collapsed && block.children.length > 0) {
            // ELSE/CASE/DEFAULT don't add a nesting level for their own children
            const depthIncrease = (block.type === 'ELSE' || block.type === 'CASE' || block.type === 'DEFAULT') ? 0 : 1
            result.push(...flattenBlocks(block.children, expandedBlockIds, depth + depthIncrease))
        }
    })

    return result
}

// ---- TreeRow: used by FlowTree / sidebar tree view ----

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

function flattenTreeBlockRows(
    blocks: Block[],
    subflowId: string,
    depth: number,
    expandedBlockIds: Set<string>,
    visibleTypes: Set<BlockType>,
    qLower?: string,
    matchedBlockIds?: Set<string>,
): TreeRow[] {
    const rows: TreeRow[] = []
    const hasSearch = !!(qLower || matchedBlockIds?.size)

    for (const block of blocks) {
        if (!visibleTypes.has(block.type)) continue

        const nameMatch = !qLower || block.name.toLowerCase().includes(qLower)
        const isMatched = matchedBlockIds?.has(block.id)
        const childRows = flattenTreeBlockRows(
            block.children,
            subflowId,
            depth + 1,
            expandedBlockIds,
            visibleTypes,
            qLower,
            matchedBlockIds,
        )
        const hasMatchingChildren = hasSearch && childRows.length > 0

        if (hasSearch && !nameMatch && !isMatched && !hasMatchingChildren) continue

        rows.push({
            kind: 'block',
            id: block.id,
            depth,
            name: block.name,
            blockType: block.type,
            childCount: block.children.length,
            hasChildren: block.children.length > 0,
            subflowId,
            blockData: block,
        })

        if (expandedBlockIds.has(block.id) || (hasSearch && hasMatchingChildren)) {
            rows.push(...childRows)
        }
    }
    return rows
}

/**
 * Flattens a full FlowDocument into sidebar tree rows, including subflow headers,
 * with optional type-filtering and search/match propagation.
 */
export function flattenTreeRows(doc: FlowDocument, options: FlattenTreeOptions): TreeRow[] {
    const {expandedSubflowIds, expandedBlockIds, visibleTypes, searchQuery, matchedBlockIds} = options
    const rows: TreeRow[] = []
    const qLower = searchQuery?.toLowerCase()
    const hasSearch = !!(qLower || matchedBlockIds?.size)

    for (const subflow of doc.subflows) {
        const sfMatch = !qLower || subflow.name.toLowerCase().includes(qLower)

        const blockRows = flattenTreeBlockRows(
            subflow.blocks,
            subflow.id,
            1,
            expandedBlockIds,
            visibleTypes,
            qLower,
            matchedBlockIds,
        )

        if (hasSearch) {
            const hasMatchingChildren = blockRows.length > 0
            if (!sfMatch && !hasMatchingChildren) continue
        }

        rows.push({
            kind: 'subflow',
            id: subflow.id,
            depth: 0,
            name: subflow.name,
            childCount: subflow.blocks.length,
            hasChildren: subflow.blocks.length > 0,
            subflowId: subflow.id,
            subflowData: subflow,
        })

        if (expandedSubflowIds.has(subflow.id) || (hasSearch && blockRows.length > 0)) {
            rows.push(...blockRows)
        }
    }
    return rows
}
