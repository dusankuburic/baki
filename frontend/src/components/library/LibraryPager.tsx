import {ChevronLeft, ChevronRight} from 'lucide-react'
import {useTranslation} from 'react-i18next'

interface Props {
  page: number
  totalPages: number
  total: number
  // The total stopped at the server's cap: more flows match, so the page count is unknown.
  totalCapped?: boolean
  // Whether flows follow this page, from a server that says so; otherwise the total decides.
  hasMore?: boolean
  // The page asked for has not answered yet: Next waits for the cursor of the page after it.
  busy?: boolean
  onChange: (page: number) => void
}

export default function LibraryPager({
  page,
  totalPages,
  total,
  totalCapped = false,
  hasMore,
  busy = false,
  onChange,
}: Props) {
  const {t} = useTranslation('library')
  const canPrev = page > 0
  const canNext = !busy && (hasMore ?? page + 1 < totalPages)
  return (
    <div className="flex items-center justify-between text-xs text-text-tertiary">
      <span>{totalCapped ? t('pager.totalCapped', {count: total}) : t('pager.total', {count: total})}</span>
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => onChange(page - 1)}
          disabled={!canPrev}
          className="inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-surface-2 disabled:opacity-40 disabled:pointer-events-none"
        >
          <ChevronLeft size={12} /> {t('pager.prev')}
        </button>
        <span className="tabular-nums">
          {totalCapped ? t('pager.page', {page: page + 1}) : t('pager.pageOf', {page: page + 1, totalPages})}
        </span>
        <button
          type="button"
          onClick={() => onChange(page + 1)}
          disabled={!canNext}
          className="inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-surface-2 disabled:opacity-40 disabled:pointer-events-none"
        >
          {t('pager.next')} <ChevronRight size={12} />
        </button>
      </div>
    </div>
  )
}
