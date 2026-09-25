import {useTranslation} from 'react-i18next'
import {StatTile, type StatDelta} from '@/components/charts'
import {healthTier} from '@/lib/scoring'
import {formatCompactIntl, formatCurrency, formatDateLong} from '@/lib/format'
import type {DashboardAnalyticsData, StatValue} from '@/types'
import {ChartSection} from '../ChartSection'

// The headline numbers, each against the previous period when comparing and
// with a sparkline across the window.
export function OverviewSection({data}: {data: DashboardAnalyticsData}) {
  const {t} = useTranslation('dashboard')
  const {summary, range, applied} = data
  const period = t('common.vsPreviousDays', {count: range.days})
  const delta = (stat: StatValue, goodWhen: StatDelta['goodWhen']): StatDelta | undefined =>
    applied.compare && stat.available && stat.prev !== null
      ? {current: stat.value, prev: stat.prev, goodWhen, periodLabel: period}
      : undefined
  const unavailable = (stat: StatValue) => (stat.available ? undefined : t('tile.unavailable'))
  const health = summary.avgHealth
  const tier = healthTier(health.value)
  const asOf = t('analytics.asOf', {date: formatDateLong(summary.asOf)})
  const inScope = [
    t('analytics.tiles.inScope', {count: summary.flowsInScope}),
    summary.unanalyzed > 0 ? t('analytics.tiles.unanalyzed', {count: summary.unanalyzed}) : '',
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <ChartSection
      id="analytics-overview"
      title={t('analytics.sections.overview')}
      description={`${t('analytics.sectionSub.overview')} ${asOf}`}
    >
      <div className="analytics-tiles analytics-span-12">
        <StatTile
          hero
          label={t('analytics.tiles.avgHealth')}
          value={String(Math.round(health.value))}
          tone={tier.text}
          sub={t('home.kpiHealthSub', {tier: t(`health.${tier.tier}`)})}
          unavailable={unavailable(health)}
          delta={delta(health, 'up')}
          spark={health.spark}
        />
        <StatTile
          label={t('analytics.tiles.openFindings')}
          value={formatCompactIntl(summary.openFindings.value)}
          sub={t('home.severityLine', {
            errors: summary.errors.value,
            warnings: summary.warnings.value,
            info: summary.info.value,
          })}
          unavailable={unavailable(summary.openFindings)}
          delta={delta(summary.openFindings, 'down')}
          spark={summary.openFindings.spark}
        />
        <StatTile
          label={t('analytics.tiles.errors')}
          value={formatCompactIntl(summary.errors.value)}
          unavailable={unavailable(summary.errors)}
          delta={delta(summary.errors, 'down')}
          spark={summary.errors.spark}
        />
        <StatTile
          label={t('analytics.tiles.autoFixable')}
          value={formatCompactIntl(summary.autoFixable.value)}
          sub={t('analytics.tiles.fixableOf', {
            available: summary.autoFixable.value,
            total: summary.openFindings.value,
          })}
          unavailable={unavailable(summary.autoFixable)}
          delta={delta(summary.autoFixable, 'neutral')}
          meter={
            summary.openFindings.value > 0
              ? {
                  value: summary.autoFixable.value,
                  max: summary.openFindings.value,
                  token: '--success',
                  label: t('analytics.tiles.meterFixable'),
                }
              : undefined
          }
        />
        <StatTile
          label={t('analytics.tiles.analysesRun')}
          value={formatCompactIntl(summary.analysesRun.value)}
          sub={t('analytics.tiles.inWindow')}
          unavailable={unavailable(summary.analysesRun)}
          delta={delta(summary.analysesRun, 'neutral')}
          spark={summary.analysesRun.spark}
        />
        <StatTile
          label={t('analytics.tiles.flowsAnalyzed')}
          value={formatCompactIntl(summary.flowsAnalyzed.value)}
          sub={inScope}
          unavailable={unavailable(summary.flowsAnalyzed)}
          delta={delta(summary.flowsAnalyzed, 'up')}
          spark={summary.flowsAnalyzed.spark}
        />
        <StatTile
          label={t('analytics.tiles.resolved')}
          value={formatCompactIntl(summary.resolved.value)}
          sub={t('analytics.tiles.inWindow')}
          unavailable={unavailable(summary.resolved)}
          delta={delta(summary.resolved, 'up')}
          spark={summary.resolved.spark}
        />
        <StatTile
          label={t('analytics.tiles.aiSpend')}
          value={formatCurrency(summary.aiSpend.value)}
          sub={t('analytics.tiles.inWindow')}
          unavailable={unavailable(summary.aiSpend)}
          delta={delta(summary.aiSpend, 'neutral')}
          spark={summary.aiSpend.spark}
        />
      </div>
      {applied.derived && <p className="analytics-span-12 text-2xs text-text-tertiary">{t('analytics.derivedNote')}</p>}
    </ChartSection>
  )
}
