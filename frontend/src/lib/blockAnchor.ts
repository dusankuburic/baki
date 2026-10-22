import type {Block, FlowDocument} from '@/types'

// A block named by content (core/models BlockAnchor): ids are minted per parse, and every applied fix or edit
// re-parses the flow. Peers are the blocks of its subflow with the same rawType and name, in line order.
export interface BlockAnchor {
  subflow: string
  rawType: string
  name: string
  ordinal: number
  count: number
}

function peersOf(doc: FlowDocument, subflow: string, rawType: string, name: string): Block[] {
  const peers: Block[] = []
  const walk = (blocks: Block[]) => {
    for (const b of blocks) {
      if (b.rawType === rawType && b.name === name) peers.push(b)
      if (b.children.length > 0) walk(b.children)
    }
  }
  for (const sf of doc.subflows) if (sf.name === subflow) walk(sf.blocks)
  return peers.sort((a, b) => a.lineNumber - b.lineNumber || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
}

function locate(doc: FlowDocument, blockId: string): {block: Block; subflow: string} | null {
  const find = (blocks: Block[]): Block | null => {
    for (const b of blocks) {
      if (b.id === blockId) return b
      const hit = b.children.length > 0 ? find(b.children) : null
      if (hit) return hit
    }
    return null
  }
  for (const sf of doc.subflows) {
    const block = find(sf.blocks)
    if (block) return {block, subflow: sf.name}
  }
  return null
}

export function anchorOf(doc: FlowDocument | null | undefined, blockId: string): BlockAnchor | null {
  const hit = doc ? locate(doc, blockId) : null
  if (!doc || !hit) return null
  const peers = peersOf(doc, hit.subflow, hit.block.rawType, hit.block.name)
  return {
    subflow: hit.subflow,
    rawType: hit.block.rawType,
    name: hit.block.name,
    ordinal: Math.max(
      0,
      peers.findIndex(p => p.id === blockId),
    ),
    count: peers.length,
  }
}

// The peer at the anchor's place while the peer count is unchanged; null otherwise: a changed count most likely
// means a fix removed this very block, and reporting it gone beats moving a conversation onto its twin.
export function resolveAnchor(doc: FlowDocument, anchor: BlockAnchor): string | null {
  const peers = peersOf(doc, anchor.subflow, anchor.rawType, anchor.name)
  if (peers.length !== anchor.count || anchor.ordinal < 0 || anchor.ordinal >= peers.length) return null
  return peers[anchor.ordinal]!.id
}

export function hasBlock(doc: FlowDocument, blockId: string): boolean {
  return locate(doc, blockId) !== null
}
