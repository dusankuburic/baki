import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {StatTile} from '@/components/charts/StatTile'
import {healthTier} from '@/lib/scoring'
import {formatCompactIntl} from '@/lib/format'
import type {
  DashboardOverview,
  DashboardFindingsAgg,
  DailyHealthPoint,
  DailySeverityPoint,
  Fixability,
} from '@/types'

export function HomeKpiRow({
  overview,
  findings,
  healthTrend,
  severityTrend,
  fixability,
  onViewFindings,
  onViewFlows,
  className,
}: {
  overview: DashboardOverview
  findings: DashboardFindingsAgg
  healthTrend: DailyHealthPoint[]
  severityTrend: DailySeverityPoint[]
  fixability: Fixability
  onViewFindings?: () => void
  onViewFlows?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const errors = findings.bySeverity.error ?? 0
  const warnings = findings.bySeverity.warning ?? 0
  const info = findings.bySeverity.info ?? 0
  const totalFindings = errors + warnings + info
  const score = overview.avgHealthScore
  const tier = healthTier(score)
  const healthSpark = healthTrend.filter(d => d.flowCount > 0).map(d => d.avgHealth)
  const findingsSpark = severityTrend.map(d => d.errors + d.warnings + d.info)

  return (
    <div className={clsx('home-kpis', className)}>
      <StatTile
        hero
        label={t('home.kpiAvgHealth')}
        value={String(score)}
        tone={tier.text}
        sub={t('home.kpiHealthSub', {tier: t(`health.${tier.tier}`)})}
        unavailable={overview.healthAvailable ? undefined : t('common.notAnalyzed')}
        spark={healthSpark}
      />
      <StatTile
        label={t('home.kpiFindings')}
        value={formatCompactIntl(totalFindings)}
        sub={t('home.severityLine', {errors, warnings, info})}
        spark={findingsSpark}
        onClick={onViewFindings}
      />
      <StatTile
        label={t('home.kpiFlows')}
        value={formatCompactIntl(overview.totalFlows)}
        sub={t('home.subflows', {count: overview.totalSubflows})}
        onClick={onViewFlows}
      />
      <StatTile
        label={t('home.kpiAutoFixable')}
        value={formatCompactIntl(fixability.available)}
        sub={
          fixability.total > 0
            ? t('home.fixableOf', {available: fixability.available, total: fixability.total})
            : t('home.rulesWithFixer', {available: fixability.autoFixableRules, total: fixability.totalRules})
        }
        unavailable={fixability.total > 0 ? undefined : t('common.notAnalyzed')}
        meter={
          fixability.total > 0
            ? {value: fixability.available, max: fixability.total, token: '--success', label: t('home.meterFixable')}
            : undefined
        }
      />
    </div>
  )
}
