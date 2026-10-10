import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
import type {ApprovalItem} from '@/stores/chatStore'
import {PatchPreviewText} from '@/components/shared'
import {ITEM_STATUS_TONE, type ApprovalPreviewProps} from '../preview'

// Takes `t` rather than calling a hook: this is a plain helper, and an unknown
// status still renders its raw wire value instead of a blank cell.
function itemStatusLine(item: ApprovalItem, t: TFunction<'chat'>): string {
  return t(`fixCard.itemStatus.${item.status}`, {defaultValue: item.status})
}

/**
 * The fix-patch preview (apply_fix / apply_fixes): the block, its line and
 * rule, and the server-rendered patch preview (already secret-scrubbed). A
 * batch renders a per-fix row list; per-item outcomes land on the rows when
 * the batch decision resolves.
 */
export default function FixPatchPreview({
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
          {batch ? t('fixCard.headingBatch', {count: approval.items.length}) : t('fixCard.heading')}
        </span>
        {batch ? (
          <span className="text-2xs text-text-tertiary">
            {pending && excluded.size > 0
              ? t('fixCard.applyingSubset', {included: includedCount, total: approval.items.length})
              : t('fixCard.selectHint')}
          </span>
        ) : (
          <code className="rounded bg-surface-3 px-1.5 py-0.5 font-mono text-2xs text-brand-300">
            {approval.fixType}
          </code>
        )}
      </div>

      {batch ? (
        <ul className="mt-2 space-y-1.5" data-testid="fix-proposal-items">
          {approval.items.map((item, i) => (
            <li key={i} className="rounded border border-border-subtle bg-surface-2/60 p-1.5">
              <div className="flex items-baseline justify-between gap-2">
                <span className="flex min-w-0 items-baseline gap-1.5">
                  {pending && (
                    <input
                      type="checkbox"
                      checked={!excluded.has(i)}
                      onChange={() => onToggle(i)}
                      className="mt-0.5 shrink-0 accent-brand-500"
                      aria-label={t('fixCard.includeFix', {name: item.blockLabel})}
                      data-testid={`include-item-${i}`}
                    />
                  )}
                  <span className="truncate text-xs text-text-secondary">
                    {item.blockLabel}
                    {(item.line ?? 0) > 0 && (
                      <span className="text-text-tertiary"> · {t('fixCard.line', {line: item.line})}</span>
                    )}
                  </span>
                </span>
                <code className="shrink-0 rounded bg-surface-3 px-1.5 py-0.5 font-mono text-2xs text-brand-300">
                  {item.fixType}
                </code>
              </div>
              {item.summary && <PatchPreviewText text={item.summary} className="mt-1 max-h-24" />}
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
      ) : (
        <>
          <div className="mt-1 text-xs text-text-secondary">
            {approval.blockLabel}
            {(approval.line ?? 0) > 0 && (
              <span className="text-text-tertiary"> · {t('fixCard.line', {line: approval.line})}</span>
            )}
            <span className="text-text-tertiary"> · {t('fixCard.rule')} </span>
            <code className="font-mono text-2xs text-text-secondary">{approval.ruleId}</code>
          </div>
          {approval.summary && <PatchPreviewText text={approval.summary} className="mt-2 max-h-40" />}
        </>
      )}
    </>
  )
}
