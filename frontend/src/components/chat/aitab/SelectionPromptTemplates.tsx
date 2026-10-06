import type {ComponentProps} from 'react'
import {PromptTemplates} from '..'
import {useSelectedBlock} from '../hooks/useSelectedBlock'

type PromptTemplatesProps = ComponentProps<typeof PromptTemplates>

export function SelectionPromptTemplates(props: Omit<PromptTemplatesProps, 'blockName'>) {
  const {selectedBlock} = useSelectedBlock()
  return <PromptTemplates {...props} blockName={selectedBlock?.name} />
}
