import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ChevronLeft, ChevronRight} from 'lucide-react'
import IconButton from './IconButton'

type PagerProps = {
  /** 0-based page index. */
  page: number
  pageSize: number
  /** Rows on the current page. */
  count: number
  /** The full match count when the server reports one. */
  total?: number
  /** Whether a next page exists, for lists whose server reports no total. */
  hasMore?: boolean
  onPageChange: (page: number) => void
  pageSizeOptions?: number[]
  onPageSizeChange?: (size: number) => void
  /** Names the pagination landmark ("Users pagination"). */
  label: string
  className?: string
}

// The pagination footer every server-paged table shares: a live "Showing
// 1–10 of 17" (or "Showing 16–30" with `hasMore` when the total is unknown),
// an optional rows-per-page select, and previous/next. Next is disabled on
// the last page either way, so a full last page never leads to an empty one.
export default function Pager({
  page,
  pageSize,
  count,
  total,
  hasMore = false,
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
              className="h-7 rounded-md border border-border-default bg-surface-2 px-1.5 text-xs text-text-primary focus:outline-none focus:ring-2 focus:ring-brand-500/20"
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
          disabled={atEnd}
        />
      </span>
    </nav>
  )
}
