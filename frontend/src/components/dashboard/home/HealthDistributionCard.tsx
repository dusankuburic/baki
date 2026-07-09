import {BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, Cell} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors, healthColor} from './useChartColors'
import type {HealthBucket} from '@/types'

// HealthDistributionCard renders the org-wide health-score histogram: five
// 20-point buckets exposing the shape the single AvgHealth number hides (e.g.
// a 70 average built from a bimodal 40/100 split). Populated in both modes.
export function HealthDistributionCard({data, className}: {data: HealthBucket[]; className?: string}) {
  const colors = useChartColors()
  const buckets = data ?? []
  const total = buckets.reduce((s, b) => s + b.count, 0)
  const chartData = buckets.map(b => ({...b, midpoint: (b.lo + b.hi) / 2}))

  return (
    <CardShell title="Health Distribution" className={className}>
      {total === 0 ? (
        <CardPlaceholder message="Analyze flows to see how health scores are distributed." />
      ) : (
        <div className="h-48" role="img" aria-label="Histogram of flow health scores in five 20-point buckets">
          <ResponsiveContainer width="100%" height="100%">
            <BarChart data={chartData} margin={{top: 10, right: 10, left: -20, bottom: 0}}>
              <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} vertical={false} />
              <XAxis
                dataKey="label"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
              />
              <YAxis
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11, fontFamily: 'var(--font-mono)'}}
                width={48}
                allowDecimals={false}
              />
              <Tooltip
                cursor={{fill: colors.surface3, fillOpacity: 0.4}}
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12, fontVariantNumeric: 'tabular-nums',
                }}
                labelFormatter={(l) => `Health ${l}`}
                formatter={(v) => [`${v} flows`, 'Count']}
              />
              <Bar dataKey="count" isAnimationActive={false} radius={[3, 3, 0, 0]}>
                {chartData.map(b => (
                  <Cell key={b.label} fill={healthColor(b.midpoint, colors)} />
                ))}
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
