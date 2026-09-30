import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
import {AlertTriangle} from 'lucide-react'
import type {LegacyDefaultNotice, ThreadModelNotice} from './hooks/useProviderSetup'

/** What the row shows: the thread's model notice, or the saved-default one. */
export type ModelNoticeView = ThreadModelNotice | ({kind: 'legacyDefault'} & LegacyDefaultNotice)

// modelNoticeSentences is the notice's copy, one translated sentence per
// entry. The row renders it, and the chat panel's always-mounted status
// region announces the same text (the row itself is not a live region, so a
// notice is announced once, when it is raised).
export function modelNoticeSentences(
  t: TFunction<'chat'>,
  notice: ModelNoticeView,
  providerName: (id: string) => string,
): string[] {
  switch (notice.kind) {
    case 'removed':
      return [
        notice.fallback
          ? t('modelNotice.removed', {wanted: notice.wanted.label, fallback: notice.fallback.label})
          : t('modelNotice.removedNone', {wanted: notice.wanted.label}),
      ]
    case 'unavailable': {
      const wanted = notice.wanted.label
      let first: string
      switch (notice.cause) {
        case 'needs_credentials':
        case 'unreadable':
          first = t('modelNotice.credentials', {wanted})
          break
        case 'invalid_config':
          first = t('modelNotice.config', {wanted})
          break
        case 'unsupported':
          first = t('modelNotice.unsupported', {wanted})
          break
        case 'provider':
          first = t('modelNotice.provider', {wanted, provider: providerName(notice.wanted.provider)})
          break
        default:
          first = t('modelNotice.unavailable', {wanted})
      }
      return notice.fallback ? [first, t('modelNotice.meanwhile', {fallback: notice.fallback.label})] : [first]
    }
    case 'refused':
      return [t('modelNotice.refused', {wanted: notice.wanted.label})]
    case 'legacyDefault':
      return [
        notice.fallback
          ? t('modelNotice.legacyDefault', {saved: notice.saved, fallback: notice.fallback})
          : t('modelNotice.legacyDefaultNone', {saved: notice.saved}),
      ]
  }
}

interface Props {
  notice: ModelNoticeView
  providerName: (id: string) => string
  /** Opens the header's model picker. */
  onChooseModel: () => void
  /**
   * Removed: pins the thread to the model it now uses. Refused: hides the
   * notice. Saved default: remembers it was seen. Never offered for a model
   * that is only temporarily unavailable — the thread keeps it and returns
   * to it once it can be used again.
   */
  onDismiss: () => void
  /** Opens AI Accounts at the models (the saved-default notice). */
  onManage: () => void
}

// ModelUnavailableNotice is the row under the chat header that says why this
// thread is not on its own model (removed, or temporarily unavailable), that
// the server refused a send for it, or that the saved Azure "Default for new
// chats" no longer names anything.
export default function ModelUnavailableNotice({notice, providerName, onChooseModel, onDismiss, onManage}: Props) {
  const {t} = useTranslation('chat')
  const sentences = modelNoticeSentences(t, notice, providerName)
  const canDismiss =
    notice.kind === 'refused' || notice.kind === 'legacyDefault' || (notice.kind === 'removed' && !!notice.fallback)
  const actionClass =
    'shrink-0 min-h-6 font-medium text-brand-400 hover:text-brand-300 transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-brand-500 rounded'

  return (
    <div
      className="flex items-start gap-2 chat-gutter py-1.5 border-b border-border-subtle bg-semantic-warning/5 text-2xs text-text-secondary"
      data-model-notice={notice.kind}
    >
      <AlertTriangle size={12} className="mt-0.5 shrink-0 text-semantic-warning" aria-hidden="true" />
      <div className="flex flex-1 min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5">
        <p className="flex-1 min-w-[10rem] [overflow-wrap:anywhere]">
          {sentences.map((s, i) => (
            <span key={i}>
              {i > 0 && ' '}
              {s}
            </span>
          ))}
        </p>
        <div className="flex items-center gap-3">
          {notice.kind === 'legacyDefault' ? (
            <button type="button" className={actionClass} onClick={onManage}>
              {t('modelNotice.manage')}
            </button>
          ) : (
            <button type="button" className={actionClass} onClick={onChooseModel}>
              {t('modelNotice.chooseModel')}
            </button>
          )}
          {canDismiss && (
            <button type="button" className={actionClass} onClick={onDismiss}>
              {t('modelNotice.dismiss')}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
