import {ChevronLeft, ChevronRight} from 'lucide-react'

interface Props {
  page: number
  totalPages: number
  total: number
  onChange: (page: number) => void
}

export default function LibraryPager({page, totalPages, total, onChange}: Props) {
  const canPrev = page > 0
  const canNext = page + 1 < totalPages
  return (
    <div className="flex items-center justify-between text-xs text-text-tertiary">
      <span>{total} flow{total === 1 ? '' : 's'} total</span>
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => onChange(page - 1)}
          disabled={!canPrev}
          className="inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-surface-2 disabled:opacity-40 disabled:pointer-events-none"
        >
          <ChevronLeft size={12} /> Prev
        </button>
        <span className="tabular-nums">
          Page {page + 1} of {totalPages}
        </span>
        <button
          type="button"
          onClick={() => onChange(page + 1)}
          disabled={!canNext}
          className="inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-surface-2 disabled:opacity-40 disabled:pointer-events-none"
        >
          Next <ChevronRight size={12} />
        </button>
      </div>
    </div>
  )
}
