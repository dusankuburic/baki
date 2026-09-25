import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Clock, AlertTriangle, Inbox} from 'lucide-react'
import {ChartCard, HBarChart, StatTile, useChartTheme, type HBarRow} from '@/components/charts'
import {formatPercent} from '@/lib/format'
import {DetailsAction} from './DetailsAction'
import type {Workflow} from '@/types'

export const TRIAGE_STATUSES = ['open', 'acknowledged', 'in_progress', 'resolved', 'suppressed'] as const
export type TriageStatusKey = (typeof TRIAGE_STATUSES)[number]

interface Row extends HBarRow {
  share: number
}

export function formatDuration(hours: number): string {
  if (hours <= 0) return '—'
  if (hours < 1) return `${Math.round(hours * 60)}m`
  if (hours < 24) return `${hours.toFixed(1)}h`
  const days = Math.floor(hours / 24)
  const remH = Math.round(hours % 24)
  return remH > 0 ? `${days}d ${remH}h` : `${days}d`
}

// How the team is working through findings: the status distribution as
// single-hue bars (states are not a funnel — findings move both ways) and the
// two numbers that say whether the backlog is moving.
export function TriageCard({data, onDetails, className}: {data: Workflow; onDetails?: () => void; className?: string}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const funnel = data.funnel ?? {}
  const total = TRIAGE_STATUSES.reduce((s, k) => s + (funnel[k] ?? 0), 0)
  const rows = useMemo<Row[]>(
    () =>
      TRIAGE_STATUSES.map(k => ({
        key: k,
        label: t(`triage.${k}`),
        value: funnel[k] ?? 0,
        share: total > 0 ? (funnel[k] ?? 0) / total : 0,
      })),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [data, total, t],
  )
  const title = t('home.triage')
  return (
    <ChartCard
      title={title}
      subtitle={t('home.triageSub')}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={data.available ? 'ready' : 'empty'}
      emptyMessage={t('triage.empty')}
      className={className}
      table={{
        caption: title,
        filename: 'triage.csv',
        columns: [
          {key: 'label', header: t('triage.status')},
          {key: 'value', header: t('triage.findings'), align: 'right'},
          {
            key: 'share',
            header: t('triage.share'),
            align: 'right',
            render: r => formatPercent(r.share),
            csv: r => formatPercent(r.share),
          },
        ],
        rows,
      }}
    >
      <HBarChart rows={rows} valueName={t('triage.findings')} color={theme.seq[2]} minHeight={140} />
      <div className="mt-3 grid grid-cols-2 gap-2">
        <StatTile
          label={t('triage.mttr')}
          value={formatDuration(data.mttrHours)}
          sub={data.resolvedCount > 0 ? t('triage.mttrSub', {count: data.resolvedCount}) : t('triage.mttrNone')}
          icon={Clock}
        />
        <StatTile
          label={t('triage.stale')}
          value={String(data.staleCount)}
          sub={t('triage.staleSub')}
          icon={data.staleCount > 0 ? AlertTriangle : Inbox}
          tone={data.staleCount > 0 ? 'text-semantic-error' : undefined}
        />
      </div>
    </ChartCard>
  )
}
