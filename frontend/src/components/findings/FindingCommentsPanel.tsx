import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Trash2} from 'lucide-react'
import {analysisApi, libraryApi} from '@/api'
import {useConfirm, useToast} from '@/components/shared'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {useAsync} from '@/hooks/useAsync'
import {useAuthStore} from '@/stores/authStore'
import {useFlowStore} from '@/stores/flowStore'
import type {FindingComment} from '@/types'
import {localDate} from '@/lib/time'

interface Props {
  comments: FindingComment[] | null
  commentLoading: boolean
  commentBody: string
  onCommentBodyChange: (value: string) => void
  onSubmit: () => void
}

export default function FindingCommentsPanel({
  comments,
  commentLoading,
  commentBody,
  onCommentBodyChange,
  onSubmit,
}: Props) {
  const {t} = useTranslation('findings')
  const toast = useToast()
  const {confirm} = useConfirm()
  const flowId = useFlowStore(s => s.document?.id ?? null)
  const userId = useAuthStore(s => s.user?.id ?? null)
  const [deletedIds, setDeletedIds] = useState<Set<string>>(() => new Set())
  const [deletingId, setDeletingId] = useState<string | null>(null)

  const visible = (comments ?? []).filter(c => !deletedIds.has(c.id))
  const needsModeration = userId !== null && visible.some(c => c.authorId !== userId)
  const {data: canModerate} = useAsync<boolean>(
    () =>
      flowId && needsModeration
        ? libraryApi
            .get(flowId)
            .then(f => f.canShare === true)
            .catch(() => false)
        : Promise.resolve(false),
    [flowId, needsModeration],
  )

  const handleDelete = async (comment: FindingComment) => {
    if (!flowId) return
    const ok = await confirm({title: t('comments.deleteTitle'), message: t('comments.deleteMessage'), danger: true})
    if (!ok) return
    setDeletingId(comment.id)
    try {
      await analysisApi.deleteComment(flowId, comment.id)
      setDeletedIds(prev => new Set(prev).add(comment.id))
    } catch (err) {
      toast.error(t('toasts.commentDeleteFailed'), {description: getErrorMessage(err)})
    } finally {
      setDeletingId(null)
    }
  }

  return (
    <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-surface-3 border border-border-subtle rounded space-y-1.5">
      <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">{t('comments.heading')}</span>
      {commentLoading ? (
        <span className="text-2xs text-text-tertiary">{t('comments.loading')}</span>
      ) : visible.length > 0 ? (
        visible.map(c => (
          <div key={c.id} className="text-2xs space-y-0.5">
            <div className="flex items-center gap-2">
              <span className="text-text-secondary font-medium">
                {/* An erased account leaves its comments with neither an id nor a name. */}
                {c.authorName || c.authorId.slice(0, 8) || t('comments.formerMember')}
              </span>
              <span className="text-text-disabled">{localDate(c.createdAt)}</span>
              {userId !== null && (c.authorId === userId || canModerate) && (
                <button
                  onClick={() => void handleDelete(c)}
                  disabled={deletingId === c.id}
                  className="ml-auto p-0.5 rounded text-text-disabled hover:text-red-500 transition-colors disabled:opacity-50"
                  aria-label={t('comments.delete')}
                  title={t('comments.delete')}
                >
                  <Trash2 className="w-3 h-3" />
                </button>
              )}
            </div>
            <p className="text-text-tertiary">{c.body}</p>
          </div>
        ))
      ) : (
        <span className="text-2xs text-text-tertiary">{t('comments.empty')}</span>
      )}
      <div className="flex items-center gap-1.5 pt-1">
        <input
          type="text"
          value={commentBody}
          onChange={e => onCommentBodyChange(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              onSubmit()
            }
          }}
          placeholder={t('comments.placeholder')}
          className="flex-1 bg-surface-2 border border-border-subtle rounded px-2 py-1 text-2xs text-text-primary placeholder:text-text-disabled focus:outline-hidden focus:border-brand-500/50"
        />
        <button
          onClick={onSubmit}
          disabled={!commentBody.trim()}
          className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors disabled:opacity-50"
        >
          {t('comments.post')}
        </button>
      </div>
    </div>
  )
}
