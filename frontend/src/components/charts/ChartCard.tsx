import {useId, useState, type ReactNode} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {BarChart3, Table2} from 'lucide-react'
import {SegmentedControl, Skeleton, ErrorState} from '@/components/shared'
import {DataTable} from './DataTable'
import type {TableSpec} from './chartTypes'

export type ChartCardState = 'ready' | 'empty' | 'error' | 'loading'

export function ChartCard<Row extends object>({
  id,
  title,
  subtitle,
  actions,
  table,
  state = 'ready',
  refetching = false,
  emptyMessage,
  errorMessage,
  onRetry,
  footnote,
  label,
  className,
  children,
}: {
  id?: string
  title: string
  subtitle?: string
  actions?: ReactNode
  table?: TableSpec<Row>
  state?: ChartCardState
  refetching?: boolean
  emptyMessage?: string
  errorMessage?: string
  onRetry?: () => void
  footnote?: string
  label?: string
  className?: string
  children?: ReactNode
}) {
  const {t} = useTranslation('dashboard')
  const uid = useId()
  const [mode, setMode] = useState<'chart' | 'table'>('chart')
  const showTable = mode === 'table' && !!table

  let body: ReactNode
  if (state === 'loading') {
    body = <Skeleton lines={3} className="py-2" />
  } else if (state === 'error') {
    body = <ErrorState message={errorMessage ?? t('common.loadFailed')} onRetry={onRetry} className="py-6" />
  } else if (state === 'empty') {
    body = <ChartPlaceholder message={emptyMessage ?? t('common.noData')} />
  } else if (showTable && table) {
    body = (
      <DataTable
        caption={table.caption}
        columns={table.columns}
        rows={table.rows}
        filename={table.filename}
        rowKey={table.rowKey}
      />
    )
  } else {
    body = (
      <div role="group" aria-roledescription={t('common.chartRole')} aria-label={label ?? title} className="min-w-0">
        {children}
      </div>
    )
  }

  return (
    <section
      id={id}
      aria-labelledby={`${uid}-title`}
      aria-busy={refetching || undefined}
      className={clsx(
        'chart-card flex min-w-0 flex-col rounded-xl border border-border-subtle bg-surface-2 p-4 shadow-sm',
        className,
      )}
    >
      <header className="mb-3 flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3
            id={`${uid}-title`}
            className="text-xs font-semibold uppercase leading-snug tracking-wider text-text-tertiary"
          >
            {title}
          </h3>
          {subtitle && <p className="mt-0.5 line-clamp-2 text-2xs text-text-tertiary">{subtitle}</p>}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {actions}
          {table && state === 'ready' && (
            <SegmentedControl
              value={mode}
              onChange={setMode}
              options={[
                {value: 'chart', label: t('common.chart'), icon: BarChart3},
                {value: 'table', label: t('common.table'), icon: Table2},
              ]}
              size="sm"
              label={t('common.viewAs', {title})}
              collapseLabels
              className="chart-card-toggle"
            />
          )}
        </div>
      </header>
      <div className={clsx('min-h-0 flex-1 transition-opacity duration-normal', refetching && 'opacity-60')}>
        {body}
      </div>
      {footnote && <p className="mt-2 text-2xs text-text-tertiary">{footnote}</p>}
    </section>
  )
}

export function ChartPlaceholder({message, className}: {message: string; className?: string}) {
  return (
    <div className={clsx('flex h-full min-h-24 w-full items-center justify-center text-center', className)}>
      <p className="max-w-[80%] text-sm text-text-tertiary">{message}</p>
    </div>
  )
}
