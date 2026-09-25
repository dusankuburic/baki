import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Download} from 'lucide-react'
import {csvCell, downloadBlob} from '@/lib/csv'
import type {TableColumn} from './chartTypes'

// The table twin of a chart: every value a chart shows is reachable here
// without hovering, and it downloads as CSV.
export function DataTable<Row extends object>({
  caption,
  columns,
  rows,
  filename,
  rowKey,
  maxRows = 200,
  className,
}: {
  caption: string
  columns: TableColumn<Row>[]
  rows: Row[]
  filename?: string
  rowKey?: (row: Row, index: number) => string
  maxRows?: number
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const shown = rows.slice(0, maxRows)

  const cellText = (row: Row, col: TableColumn<Row>): string => {
    if (col.csv) return col.csv(row)
    const v = (row as Record<string, unknown>)[col.key]
    return v === null || v === undefined ? '' : String(v)
  }

  const download = () => {
    if (!filename) return
    const lines = [columns.map(c => csvCell(c.header)).join(',')]
    for (const row of rows) lines.push(columns.map(c => csvCell(cellText(row, c))).join(','))
    downloadBlob(lines.join('\n'), 'text/csv;charset=utf-8', filename)
  }

  return (
    <div className={clsx('chart-table relative', className)}>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <caption className="sr-only">{caption}</caption>
          <thead>
            <tr className="border-b border-border-subtle text-text-tertiary">
              {columns.map(c => (
                <th
                  key={c.key}
                  scope="col"
                  className={clsx('py-1.5 pr-3 font-medium', c.align === 'right' ? 'text-right' : 'text-left')}
                >
                  {c.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {shown.map((row, i) => (
              <tr key={rowKey ? rowKey(row, i) : i} className="border-b border-border-subtle/60 last:border-0">
                {columns.map(c => (
                  <td
                    key={c.key}
                    className={clsx(
                      'py-1.5 pr-3 text-text-primary',
                      c.align === 'right' ? 'text-right tabular-nums' : 'text-left',
                    )}
                  >
                    {c.render ? c.render(row) : cellText(row, c)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {(rows.length > shown.length || filename) && (
        <div className="mt-2 flex items-center justify-between gap-2 text-2xs text-text-tertiary">
          <span>
            {rows.length > shown.length ? t('table.truncated', {shown: shown.length, total: rows.length}) : ''}
          </span>
          {filename && (
            <button
              type="button"
              onClick={download}
              className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-medium text-brand-400 hover:text-brand-300 transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500/40"
            >
              <Download size={12} aria-hidden="true" />
              {t('table.csv')}
            </button>
          )}
        </div>
      )}
    </div>
  )
}
