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
      toast.error(t('toasts.commentFailed'), {description: String(err)})
    }
  }, [commentBody, doc, finding, toast, t])

  return {showComments, comments, commentBody, setCommentBody, commentLoading, handleComments, handleSubmitComment}
}
