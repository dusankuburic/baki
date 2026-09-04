import {useTranslation} from 'react-i18next'
import type {FindingComment} from '@/types'

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
  return (
    <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-surface-3 border border-border-subtle rounded space-y-1.5">
      <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">{t('comments.heading')}</span>
      {commentLoading ? (
        <span className="text-2xs text-text-tertiary">{t('comments.loading')}</span>
      ) : comments && comments.length > 0 ? (
        comments.map(c => (
          <div key={c.id} className="text-2xs space-y-0.5">
            <div className="flex items-center gap-2">
              <span className="text-text-secondary font-medium">{c.authorName || c.authorId.slice(0, 8)}</span>
              <span className="text-text-disabled">{new Date(c.createdAt).toLocaleDateString()}</span>
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
          className="flex-1 bg-surface-2 border border-border-subtle rounded px-2 py-1 text-2xs text-text-primary placeholder:text-text-disabled focus:outline-none focus:border-brand-500/50"
        />
        <button
          onClick={onSubmit}
          disabled={!commentBody.trim()}
          className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors disabled:opacity-50"
        >
          Post
        </button>
      </div>
    </div>
  )
}
