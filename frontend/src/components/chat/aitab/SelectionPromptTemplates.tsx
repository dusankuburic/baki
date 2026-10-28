import type {ComponentProps} from 'react'
import {PromptTemplates} from '..'
import {useBlockInfo} from '../hooks/useSelectedBlock'

type PromptTemplatesProps = ComponentProps<typeof PromptTemplates>

// The templates name the thread's context block (what the request is grounded on), never the canvas selection
// (R-frontend-7). A context block no longer in the flow offers no block template.
export function SelectionPromptTemplates({
  contextBlockId,
  ...props
}: Omit<PromptTemplatesProps, 'blockName' | 'hasBlock'> & {contextBlockId: string | null}) {
  const contextBlock = useBlockInfo(contextBlockId)
  return <PromptTemplates {...props} hasBlock={!!contextBlock} blockName={contextBlock?.name} />
}
