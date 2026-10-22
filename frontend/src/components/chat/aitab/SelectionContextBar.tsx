import type {ComponentProps} from 'react'
import ChatContextBar from '../ChatContextBar'
import {useBlockInfo} from '../hooks/useSelectedBlock'
import {useFlowStore} from '@/stores/flowStore'

type ContextBarProps = ComponentProps<typeof ChatContextBar>

// The chip names the thread's context block, not the canvas selection: the two differ whenever the user
// selects another block after scoping the thread.
export function SelectionContextBar(props: Omit<ContextBarProps, 'blockName' | 'blockType' | 'selectedBlockId'>) {
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const contextBlock = useBlockInfo(props.contextBlockId)
  return (
    <ChatContextBar
      {...props}
      blockName={contextBlock?.name}
      blockType={contextBlock?.rawType}
      selectedBlockId={selectedBlockId}
    />
  )
}
