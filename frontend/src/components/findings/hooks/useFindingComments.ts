import {useTranslation} from 'react-i18next'
import {useState, useCallback} from 'react'
import {analysisApi} from '@/api'
import {useToast} from '@/components/shared'
import {findingKey} from '@/stores/analysisStore'
import type {Finding, FlowDocument, FindingComment} from '@/types'

export function useFindingComments(finding: Finding, doc: FlowDocument | null) {
  const {t} = useTranslation('findings')
  const toast = useToast()
  const [showComments, setShowComments] = useState(false)
  const [comments, setComments] = useState<FindingComment[] | null>(null)
  const [commentBody, setCommentBody] = useState('')
  const [commentLoading, setCommentLoading] = useState(false)

  const handleComments = useCallback(async () => {
    if (showComments) {
      setShowComments(false)
      return
    }
    setShowComments(true)
    if (comments === null && !commentLoading) {
      setCommentLoading(true)
      try {
        const key = findingKey(finding)
        const result = await analysisApi.listComments(doc?.id ?? '', key)
        setComments(result || [])
      } catch {
        setComments([])
      } finally {
        setCommentLoading(false)
      }
    }
  }, [showComments, comments, commentLoading, finding, doc])

  const handleSubmitComment = useCallback(async () => {
    if (!commentBody.trim() || !doc) return
    const body = commentBody.trim()
    setCommentBody('')
    try {
      const key = findingKey(finding)
      const comment = await analysisApi.addComment(doc.id, key, body)
      setComments(prev => [...(prev ?? []), comment])
    } catch (err) {
      // Put back what was typed. The composer is cleared optimistically before
      // the await, and there is no draft store behind it, so a 500/403/timeout
      // otherwise destroys the text outright — for a triager that can be a
      // multi-sentence risk-acceptance justification with nowhere to recover it
      // from. Only restore when the composer is still empty: if the user
      // started typing again while the request was in flight, their newer text
      // wins.
      setCommentBody(current => (current.trim() === '' ? body : current))
      toast.error(t('toasts.commentFailed'), {description: String(err)})
    }
  }, [commentBody, doc, finding, toast, t])

  return {showComments, comments, commentBody, setCommentBody, commentLoading, handleComments, handleSubmitComment}
}
