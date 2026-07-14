import {useMemo} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {flattenBlocks} from '@/lib/tree'

export type {FlatBlock} from '@/lib/tree'

export function useFlattenedBlocks(subflowId?: string) {
  const document = useFlowStore(s => s.document)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
  const expandedBlockIds = useFlowStore(s => s.expandedBlockIds)

  const subflow = useMemo(() => {
    const id = subflowId ?? selectedSubflowId
    return document?.subflows.find(s => s.id === id) ?? document?.subflows[0]
  }, [document, subflowId, selectedSubflowId])

  return useMemo(() => (subflow ? flattenBlocks(subflow.blocks, expandedBlockIds) : []), [subflow, expandedBlockIds])
}
