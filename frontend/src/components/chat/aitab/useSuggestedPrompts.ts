import {useEffect, useState} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'

// The suggested prompts under the welcome copy: loaded from the backend
// whenever the canvas selection or the findings presence changes (they shape
// which prompts are useful).
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
