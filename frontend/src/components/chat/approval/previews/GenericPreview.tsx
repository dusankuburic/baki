import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
import type {ApprovalItem} from '@/stores/chatStore'
import {ITEM_STATUS_TONE, type ApprovalPreviewProps} from '../preview'

function itemStatusLine(item: ApprovalItem, t: TFunction<'chat'>): string {
  return t(`approvalCard.itemStatus.${item.status}`, {defaultValue: item.status})
}

/**
 * The preview for a request of a kind this bundle does not know (a tool a
 * newer server added): the tool's name, the scope it needs, and each item's
 * label with its summary as plain text. Nothing is interpreted — the server
 * wrote the summaries for a reader — and the decision is the frame's.
 */
export default function GenericPreview({
  approval,
  pending,
  batch,
  excluded,
  includedCount,
  onToggle,
}: ApprovalPreviewProps) {
  const {t} = useTranslation('chat')
  return (
    <>
      <div className="flex items-center justify-between gap-2">
        <span className="text-2xs font-semibold uppercase tracking-wide text-brand-300">
          {t('approvalCard.heading')}
        </span>
        <code className="rounded bg-surface-3 px-1.5 py-0.5 font-mono text-2xs text-brand-300">{approval.tool}</code>
      </div>
      {(approval.requiredScope || batch) && (
        <div className="mt-1 flex items-baseline justify-between gap-2 text-2xs text-text-tertiary">
          <span>{approval.requiredScope ? t('approvalCard.scope', {scope: approval.requiredScope}) : ''}</span>
          {batch && (
            <span>
              {pending && excluded.size > 0
                ? t('approvalCard.approvingSubset', {included: includedCount, total: approval.items.length})
                : t('approvalCard.selectHint')}
            </span>
          )}
        </div>
      )}

      <ul className="mt-2 space-y-1.5" data-testid="fix-proposal-items">
        {approval.items.map((item, i) => (
          <li key={i} className="rounded border border-border-subtle bg-surface-2/60 p-1.5">
            <span className="flex min-w-0 items-baseline gap-1.5">
              {pending && batch && (
                <input
                  type="checkbox"
                  checked={!excluded.has(i)}
                  onChange={() => onToggle(i)}
                  className="mt-0.5 shrink-0 accent-brand-500"
                  aria-label={t('approvalCard.includeItem', {name: item.label})}
                  data-testid={`include-item-${i}`}
                />
              )}
              <span className="truncate text-xs text-text-secondary">{item.label}</span>
            </span>
            {/* Bounded like the fix preview: the summary's length is the server's. */}
            {item.summary && (
              <p className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words text-2xs text-text-tertiary">
                {item.summary}
              </p>
            )}
            {item.status !== 'pending' && (
              <p
                className={'mt-1 text-2xs ' + (ITEM_STATUS_TONE[item.status] ?? 'text-text-tertiary')}
                data-testid="fix-item-status"
              >
                {itemStatusLine(item, t)}
                {item.message ? ` — ${item.message}` : ''}
              </p>
            )}
          </li>
        ))}
      </ul>
    </>
  )
}
