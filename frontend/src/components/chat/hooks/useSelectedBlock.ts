import {useMemo} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {buildBlockLookup, type BlockLookup} from '@/lib/tree'
import type {FlowDocument} from '@/types'

// One index per document, shared by every subscriber: the lookup is a walk of
// the whole tree, and two components wanting the selected block's name should
// not each walk it.
const lookups = new WeakMap<FlowDocument, BlockLookup>()

function lookupFor(doc: FlowDocument): BlockLookup {
  let lookup = lookups.get(doc)
  if (!lookup) {
    lookup = buildBlockLookup(doc)
    lookups.set(doc, lookup)
  }
  return lookup
}

// The canvas selection and the selected block's meta, for the few chat
// controls that show it (the context bar, the prompt templates). Only those
// subscribe: the chat panel itself must NOT follow the selection, or every
// arrow key in the editor re-renders the whole conversation.
export function useSelectedBlock() {
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const document = useFlowStore(s => s.document)
  const selectedBlock = useMemo(
    () => (document && selectedBlockId ? (lookupFor(document).get(selectedBlockId) ?? null) : null),
    [document, selectedBlockId],
  )
  return {selectedBlockId, selectedBlock}
}
