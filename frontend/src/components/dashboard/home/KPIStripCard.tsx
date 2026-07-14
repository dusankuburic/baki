import clsx from 'clsx'
import {useChartColors, healthColor} from './useChartColors'
import {formatCount} from '@/lib/format'
import type {DashboardOverview, DashboardFindingsAgg} from '@/types'

interface KPI {
  label: string
  value: string | number
  sub?: string
  accent?: string
}

export function KPIStripCard({
  overview,
  findings,
  costByProvider,
  className,
}: {
  overview: DashboardOverview
  findings: DashboardFindingsAgg
  costByProvider: {provider: string; cost: number}[]
  className?: string
}) {
  const colors = useChartColors()
  const totalFindings =
    (findings.bySeverity.error ?? 0) + (findings.bySeverity.warning ?? 0) + (findings.bySeverity.info ?? 0)
  const totalCost = costByProvider.reduce((s, p) => s + p.cost, 0)
  const score = overview.avgHealthScore
  const scoreColor = overview.healthAvailable ? healthColor(score, colors) : colors.textTertiary

  const kpis: KPI[] = [
    {
      label: 'Avg Health',
      value: overview.healthAvailable ? score : '—',
      sub: overview.healthAvailable ? 'of 100' : 'not analyzed',
      accent: scoreColor,
    },
    {
      label: 'Findings',
      value: formatCount(totalFindings),
      sub: `${findings.bySeverity.error ?? 0}E · ${findings.bySeverity.warning ?? 0}W · ${findings.bySeverity.info ?? 0}I`,
    },
    {
      label: 'Flows',
      value: formatCount(overview.totalFlows),
      sub: `${formatCount(overview.totalSubflows)} subflows`,
    },
  ]

  if (costByProvider.length > 0) {
    kpis.splice(2, 0, {
      label: 'AI Spend (30d)',
      value: totalCost > 0 ? `$${totalCost.toFixed(2)}` : '—',
      sub: totalCost > 0 ? `${costByProvider.length} providers` : 'no usage',
    })
  }

  return (
    <div
      className={clsx(
        'grid gap-3',
        className,
        costByProvider.length > 0 ? 'grid-cols-2 lg:grid-cols-4' : 'grid-cols-3',
      )}
    >
      {kpis.map(kpi => (
        <div key={kpi.label} className="bg-surface-2 border border-border-subtle rounded-xl p-3 flex flex-col gap-0.5">
          <span className="text-2xs uppercase tracking-wider text-text-tertiary">{kpi.label}</span>
          <span
            className="text-2xl font-black font-mono tabular-nums"
            style={kpi.accent ? {color: kpi.accent} : undefined}
          >
            {kpi.value}
          </span>
          {kpi.sub && <span className="text-2xs text-text-tertiary tabular-nums">{kpi.sub}</span>}
        </div>
      ))}
    </div>
  )
}
