import {useState, useCallback} from 'react'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import type {Finding} from '@/types'

export function useRelatedFindings(finding: Finding) {
  const [showRelated, setShowRelated] = useState(false)
  const [related, setRelated] = useState<Finding[] | null>(null)
  const [relatedLoading, setRelatedLoading] = useState(false)
  const [relatedError, setRelatedError] = useState(false)

  const fetchRelated = useCallback(async () => {
    setRelatedLoading(true)
    setRelatedError(false)
    try {
      const result = await analysisApi.getRelatedFindings(finding.blockId)
      setRelated(result.filter(f => f.id !== finding.id))
    } catch (err) {
      logger.warn('Failed to load related findings', err)
      setRelatedError(true)
    } finally {
      setRelatedLoading(false)
    }
  }, [finding.id, finding.blockId])

  const handleRelated = useCallback(() => {
    if (showRelated) {
      setShowRelated(false)
      return
    }
    setShowRelated(true)
    if (related === null && !relatedLoading) fetchRelated()
  }, [showRelated, related, relatedLoading, fetchRelated])

  return {showRelated, related, relatedLoading, relatedError, handleRelated, fetchRelated}
}
