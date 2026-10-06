import type {ComponentProps} from 'react'
import ChatContextBar from '../ChatContextBar'
import {useSelectedBlock} from '../hooks/useSelectedBlock'

// The two controls that name the selected block subscribe to the canvas
// selection themselves (useSelectedBlock), so a selection change re-renders
// only them, not the panel above.
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
