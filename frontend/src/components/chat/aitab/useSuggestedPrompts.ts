import {useEffect, useState} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'

export function useSuggestedPrompts(hasSelectedBlock: boolean, hasFindings: boolean): string[] {
  const [suggestedPrompts, setSuggestedPrompts] = useState<string[]>([])

  useEffect(() => {
    let active = true
    chatApi
      .getSuggestedPrompts(hasSelectedBlock, hasFindings)
      .then((ps: string[] | null) => {
        if (active) setSuggestedPrompts(ps || [])
      })
      .catch(err => {
        logger.warn('Failed to load suggested prompts', err)
      })
    return () => {
      active = false
    }
  }, [hasSelectedBlock, hasFindings])

  return suggestedPrompts
}
