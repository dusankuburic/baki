import type {ComponentProps} from 'react'
import ChatContextBar from '../ChatContextBar'
import {useSelectedBlock} from '../hooks/useSelectedBlock'

type ContextBarProps = ComponentProps<typeof ChatContextBar>

export function SelectionContextBar(props: Omit<ContextBarProps, 'blockName' | 'blockType' | 'selectedBlockId'>) {
  const {selectedBlockId, selectedBlock} = useSelectedBlock()
  return (
    <ChatContextBar
      {...props}
      blockName={selectedBlock?.name}
      blockType={selectedBlock?.rawType}
      selectedBlockId={selectedBlockId}
    />
  )
}
