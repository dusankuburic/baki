import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ChevronLeft, ChevronRight} from 'lucide-react'
import IconButton from './IconButton'

type PagerProps = {
  page: number
  pageSize: number
  count: number
  total?: number
  hasMore?: boolean
  // Next stays disabled whatever the count says: a page still loading or failed (its next page's cursor is not known),
  // or a server saying no page follows that it can reach.
  nextDisabled?: boolean
  onPageChange: (page: number) => void
  pageSizeOptions?: number[]
  onPageSizeChange?: (size: number) => void
  label: string
  className?: string
}

export default function Pager({
  page,
  pageSize,
  count,
  total,
  hasMore = false,
  nextDisabled = false,
  onPageChange,
  pageSizeOptions,
  onPageSizeChange,
  label,
  className,
}: PagerProps) {
  const {t} = useTranslation()
  const from = count === 0 ? 0 : page * pageSize + 1
  const to = page * pageSize + count
  const atEnd = total !== undefined ? (page + 1) * pageSize >= total : !hasMore
  const status =
    count === 0
      ? t('pager.empty')
      : total !== undefined
        ? t('pager.showingOf', {from, to, total})
        : t('pager.showing', {from, to})

  return (
    <nav
      aria-label={label}
      className={clsx(
        'flex flex-wrap items-center justify-between gap-x-4 gap-y-2 text-xs text-text-tertiary',
        className,
      )}
    >
      <span aria-live="polite" className="tabular-nums">
        {status}
      </span>
      <span className="flex items-center gap-2">
        {pageSizeOptions && onPageSizeChange && (
          <label className="flex items-center gap-1.5">
            <span>{t('pager.pageSize')}</span>
            <select
              value={pageSize}
              onChange={e => onPageSizeChange(Number(e.target.value))}
              className="h-7 rounded-md border border-border-default bg-surface-2 px-1.5 text-xs text-text-primary focus:outline-hidden focus:ring-2 focus:ring-brand-500/20"
            >
              {pageSizeOptions.map(n => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        )}
        <IconButton
          icon={ChevronLeft}
          size="sm"
          label={t('pager.previous')}
          onClick={() => onPageChange(page - 1)}
          disabled={page === 0}
        />
        <IconButton
          icon={ChevronRight}
          size="sm"
          label={t('pager.next')}
          onClick={() => onPageChange(page + 1)}
          disabled={atEnd || nextDisabled}
        />
      </span>
    </nav>
  )
}
