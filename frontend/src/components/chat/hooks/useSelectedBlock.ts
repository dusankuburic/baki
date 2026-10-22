import {useMemo} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {buildBlockLookup, type BlockLookup} from '@/lib/tree'
import type {FlowDocument} from '@/types'

const lookups = new WeakMap<FlowDocument, BlockLookup>()

function lookupFor(doc: FlowDocument): BlockLookup {
  let lookup = lookups.get(doc)
  if (!lookup) {
    lookup = buildBlockLookup(doc)
    lookups.set(doc, lookup)
  }
  return lookup
}

export function useSelectedBlock() {
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const document = useFlowStore(s => s.document)
  const selectedBlock = useMemo(
    () => (document && selectedBlockId ? (lookupFor(document).get(selectedBlockId) ?? null) : null),
    [document, selectedBlockId],
  )
  return {selectedBlockId, selectedBlock}
}

export function useBlockInfo(blockId: string | null) {
  const document = useFlowStore(s => s.document)
  return useMemo(() => (document && blockId ? (lookupFor(document).get(blockId) ?? null) : null), [document, blockId])
}
