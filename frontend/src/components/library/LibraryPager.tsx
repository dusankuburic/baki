import {ChevronLeft, ChevronRight} from 'lucide-react'
import {useTranslation} from 'react-i18next'

interface Props {
  page: number
  totalPages: number
  total: number
  onChange: (page: number) => void
}

export default function LibraryPager({page, totalPages, total, onChange}: Props) {
  const {t} = useTranslation('library')
  const canPrev = page > 0
  const canNext = page + 1 < totalPages
  return (
    <div className="flex items-center justify-between text-xs text-text-tertiary">
      <span>{t('pager.total', {count: total})}</span>
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => onChange(page - 1)}
          disabled={!canPrev}
          className="inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-surface-2 disabled:opacity-40 disabled:pointer-events-none"
        >
          <ChevronLeft size={12} /> {t('pager.prev')}
        </button>
        <span className="tabular-nums">{t('pager.pageOf', {page: page + 1, totalPages})}</span>
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
