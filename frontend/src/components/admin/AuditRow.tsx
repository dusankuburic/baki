import {Fragment, useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Check, ChevronDown, Copy} from 'lucide-react'
import type {AuditEvent} from '@/api/admin'
import {IconButton} from '@/components/shared'
import {actionIcon, actionLabel} from '@/components/dashboard/home/activity'
import {useCopy} from '@/hooks/useCopy'
import {formatDateTime} from '@/lib/format'
import {relativeTimeText} from '@/lib/time'
import {actionTone} from './auditActions'

function renderActionIcon(action: string) {
  const Icon = actionIcon(action)
  return <Icon size={11} aria-hidden="true" />
}

export function AuditRow({
  event,
  labels,
  onFilterUser,
}: {
  event: AuditEvent
  labels: Record<string, string>
  onFilterUser: (userId: string) => void
}) {
  const {t} = useTranslation('admin')
  const [open, setOpen] = useState(false)
  const {copied, copy} = useCopy()
  const subjectId = !event.userId && event.resourceType === 'user' ? event.resourceId : ''
  const actor = event.email || event.userId
  const filterTarget = event.userId || subjectId
  const metaEntries = Object.entries(event.meta ?? {})
  const detailsId = `audit-${event.id}-details`

  return (
    <>
      <tr className="transition-colors duration-fast hover:bg-surface-3/60">
        <td className="whitespace-nowrap px-4 py-2 text-xs tabular-nums text-text-secondary">
          <time dateTime={event.createdAt} title={relativeTimeText(event.createdAt)}>
            {formatDateTime(event.createdAt)}
          </time>
        </td>
        <td className="whitespace-nowrap px-4 py-2 text-xs">
          {filterTarget ? (
            <button
              type="button"
              onClick={() => onFilterUser(filterTarget)}
              title={t('audit.filterByUser', {user: actor || subjectId})}
              className="max-w-[16rem] truncate rounded-sm text-left text-text-primary hover:text-brand-400 hover:underline focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
            >
              {actor || t('audit.subjectUser', {id: subjectId.slice(0, 8)})}
            </button>
          ) : (
            <span className="text-text-tertiary">{t('audit.noUser')}</span>
          )}
        </td>
        <td className="whitespace-nowrap px-4 py-2">
          <span
            className={clsx(
              'inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-2xs font-medium',
              actionTone(event.action),
            )}
            title={event.action}
          >
            {renderActionIcon(event.action)}
            {actionLabel(labels, event.action)}
          </span>
        </td>
        <td className="whitespace-nowrap px-4 py-2 text-xs">
          {event.resourceType ? (
            <span className="inline-flex items-center gap-1 font-mono text-text-tertiary">
              <span title={event.resourceId}>
                {event.resourceType}/{event.resourceId.slice(0, 8)}
              </span>
              {event.resourceId && (
                <IconButton
                  icon={copied ? Check : Copy}
                  size="sm"
                  label={copied ? t('audit.copied') : t('audit.copyResource')}
                  onClick={() => copy(event.resourceId)}
                />
              )}
            </span>
          ) : (
            <span className="text-text-tertiary">—</span>
          )}
        </td>
        <td className="whitespace-nowrap px-4 py-2 font-mono text-xs text-text-tertiary">{event.ip || '—'}</td>
        <td className="px-2 py-2 text-right">
          <button
            type="button"
            aria-expanded={open}
            aria-controls={detailsId}
            aria-label={open ? t('audit.hideDetails') : t('audit.showDetails')}
            onClick={() => setOpen(o => !o)}
            className="inline-flex h-6 w-6 items-center justify-center rounded-md text-text-tertiary transition-colors duration-fast hover:bg-surface-3 hover:text-text-primary focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
          >
            <ChevronDown
              size={14}
              className={clsx('transition-transform duration-fast', open && 'rotate-180')}
              aria-hidden="true"
            />
          </button>
        </td>
      </tr>
      {open && (
        <tr id={detailsId} className="bg-surface-1/60">
          <td colSpan={6} className="px-4 py-2">
            {metaEntries.length === 0 ? (
              <span className="text-xs text-text-tertiary">{t('audit.noMeta')}</span>
            ) : (
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
                {metaEntries.map(([k, v]) => (
                  <Fragment key={k}>
                    <dt className="font-mono text-text-tertiary">{k}</dt>
                    <dd className="break-all text-text-secondary">{v}</dd>
                  </Fragment>
                ))}
              </dl>
            )}
          </td>
        </tr>
      )}
    </>
  )
}
