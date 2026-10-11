import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ArrowUpRight, ArrowDownRight, ExternalLink} from 'lucide-react'
import {ChartCard, DataTable, Meter} from '@/components/charts'
import {healthTier} from '@/lib/scoring'
import {relativeTimeText, absoluteTime} from '@/lib/time'
import type {DashboardAnalyticsData, FlowPoint} from '@/types'
import {HealthDistributionCard} from '../../home/HealthDistributionCard'
import {FlowComplexityCard} from '../../home/FlowComplexityCard'
import {ChartSection} from '../ChartSection'

export function FlowsSection({
  data,
  refetching,
  onOpenFlow,
}: {
  data: DashboardAnalyticsData
  refetching: boolean
  onOpenFlow: (id: string) => void
}) {
  const {t} = useTranslation('dashboard')
  const complexity = useMemo(
    () =>
      data.flows.complexity.map(p => ({
        id: p.flowId,
        name: p.name,
        blocks: p.blockCount,
        findings: p.findings,
        health: p.health,
      })),
    [data.flows.complexity],
  )
  const failed = data.partial.includes('findings')
  const worst = data.flows.worst
  const movers = data.flows.movers
  const compare = data.applied.compare

  return (
    <ChartSection
      id="analytics-flows"
      title={t('analytics.sections.flows')}
      description={t('analytics.sectionSub.flows')}
    >
      <HealthDistributionCard
        data={data.flows.healthBuckets}
        title={t('analytics.cards.healthDistribution')}
        subtitle={t('analytics.cards.healthDistributionSub')}
        refetching={refetching}
        className="analytics-span-5"
      />
      <FlowComplexityCard
        rows={complexity}
        title={t('analytics.cards.complexity')}
        subtitle={t('analytics.cards.complexitySub')}
        refetching={refetching}
        className="analytics-span-7"
      />
      <ChartCard
        title={t('analytics.cards.worst')}
        subtitle={t('analytics.cards.worstSub')}
        state={failed ? 'error' : worst.length > 0 ? 'ready' : 'empty'}
        refetching={refetching}
        emptyMessage={t('analytics.cards.worstEmpty')}
        className="analytics-span-7"
      >
        <DataTable<FlowPoint>
          caption={t('analytics.cards.worst')}
          filename="flows-needing-attention.csv"
          rowKey={r => r.flowId}
          columns={[
            {
              key: 'name',
              header: t('analytics.columns.flow'),
              render: r => (
                <button
                  type="button"
                  onClick={() => onOpenFlow(r.flowId)}
                  className="inline-flex max-w-full items-center gap-1 truncate text-left font-medium text-text-primary hover:text-brand-400"
                  aria-label={`${t('common.open')} ${r.name}`}
                >
                  <span className="truncate">{r.name}</span>
                  <ExternalLink size={11} aria-hidden="true" className="shrink-0 text-text-tertiary" />
                </button>
              ),
            },
            {
              key: 'health',
              header: t('analytics.columns.health'),
              align: 'right',
              render: r => {
                const tier = healthTier(r.health)
                return (
                  <span className="inline-flex items-center justify-end gap-2">
                    <span className="w-16 shrink-0">
                      <Meter value={r.health} max={100} token={tier.token} label={t(`health.${tier.tier}`)} />
                    </span>
                    <span className={clsx('w-7 text-right font-semibold tabular-nums', tier.text)}>{r.health}</span>
                  </span>
                )
              },
              csv: r => String(r.health),
            },
            {key: 'errors', header: t('analytics.columns.errors'), align: 'right'},
            {key: 'warnings', header: t('analytics.columns.warnings'), align: 'right'},
            {key: 'info', header: t('analytics.columns.info'), align: 'right'},
            {
              key: 'analyzedAt',
              header: t('analytics.columns.analyzed'),
              render: r => (
                <span title={absoluteTime(r.analyzedAt)} className="whitespace-nowrap">
                  {relativeTimeText(r.analyzedAt)}
                </span>
              ),
              csv: r => r.analyzedAt,
            },
          ]}
          rows={worst}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.movers')}
        subtitle={t('analytics.cards.moversSub')}
        state={failed ? 'error' : compare && movers.length > 0 ? 'ready' : 'empty'}
        refetching={refetching}
        emptyMessage={compare ? t('analytics.cards.moversEmpty') : t('analytics.cards.moversNeedCompare')}
        className="analytics-span-5"
      >
        <ul className="divide-y divide-border-subtle/60">
          {movers.map(m => {
            const up = m.delta > 0
            const Icon = up ? ArrowUpRight : ArrowDownRight
            return (
              <li key={m.flowId} className="flex items-center gap-3 py-1.5 text-xs">
                <button
                  type="button"
                  onClick={() => onOpenFlow(m.flowId)}
                  className="min-w-0 flex-1 truncate text-left text-text-primary hover:text-brand-400"
                >
                  {m.name}
                </button>
                <span className="shrink-0 tabular-nums text-text-tertiary">
                  {m.healthFrom ?? '—'} → {m.healthTo ?? '—'}
                </span>
                <span
                  className={clsx(
                    'inline-flex w-12 shrink-0 items-center justify-end gap-0.5 font-semibold tabular-nums',
                    up ? 'text-semantic-success' : 'text-semantic-error',
                  )}
                >
                  <Icon size={12} aria-hidden="true" />
                  {up ? '+' : ''}
                  {m.delta}
                </span>
              </li>
            )
          })}
        </ul>
      </ChartCard>
    </ChartSection>
  )
}
