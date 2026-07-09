import {AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, Legend} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import {formatCompact} from '@/lib/format'
import type {DailySeverityPoint} from '@/types'

// SeverityTrendCard plots the daily org-wide error/warning/info counts as a
// stacked area — the "is my fleet getting healthier over time?" chart. Cloud-
// only: the underlying time series comes from flow_analysis_history, which
// desktop mode doesn't persist.
export function SeverityTrendCard({data, className}: {data: DailySeverityPoint[]; className?: string}) {
  const colors = useChartColors()
  const rows = data ?? []
  const hasData = rows.length > 0 && rows.some(d => d.errors + d.warnings + d.info > 0)

  return (
    <CardShell title="Severity Trend · 30d" className={className}>
      {rows.length === 0 ? (
        <CardPlaceholder message="Severity trend appears after you analyze flows across multiple sessions." />
      ) : !hasData ? (
        <CardPlaceholder message="No findings in the last 30 days." />
      ) : (
        <div className="h-48" role="img" aria-label="Stacked area chart of daily error, warning, and info finding counts over the last 30 days">
          <ResponsiveContainer width="100%" height="100%">
            <AreaChart data={rows} margin={{top: 10, right: 10, left: -20, bottom: 0}}>
              <defs>
                <linearGradient id="dashSevError" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={colors.error} stopOpacity={0.5} />
                  <stop offset="95%" stopColor={colors.error} stopOpacity={0} />
                </linearGradient>
                <linearGradient id="dashSevWarn" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={colors.warning} stopOpacity={0.5} />
                  <stop offset="95%" stopColor={colors.warning} stopOpacity={0} />
                </linearGradient>
                <linearGradient id="dashSevInfo" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={colors.brand400} stopOpacity={0.5} />
                  <stop offset="95%" stopColor={colors.brand400} stopOpacity={0} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} vertical={false} />
              <XAxis
                dataKey="date"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                tickFormatter={(d: string) => d.slice(5)}
                minTickGap={16}
              />
              <YAxis
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11, fontFamily: 'var(--font-mono)'}}
                width={48}
                tickFormatter={formatCompact}
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12, fontVariantNumeric: 'tabular-nums',
                }}
                labelStyle={{color: 'var(--text-primary)'}}
              />
              <Legend wrapperStyle={{fontSize: 11, color: 'var(--text-tertiary)'}} />
              {/* A one-day series has no horizontal extent, so the stacked
                  ribbons are invisible — render dots so the lone day still shows. */}
              <Area type="monotone" dataKey="errors" name="Errors" stackId="1" stroke={colors.error} fill="url(#dashSevError)" dot={rows.length === 1} isAnimationActive={false} />
              <Area type="monotone" dataKey="warnings" name="Warnings" stackId="1" stroke={colors.warning} fill="url(#dashSevWarn)" dot={rows.length === 1} isAnimationActive={false} />
              <Area type="monotone" dataKey="info" name="Info" stackId="1" stroke={colors.brand400} fill="url(#dashSevInfo)" dot={rows.length === 1} isAnimationActive={false} />
            </AreaChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
