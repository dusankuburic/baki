import {LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, ReferenceArea} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {DailyHealthPoint} from '@/types'

export function HealthTrendCard({data, className}: {data: DailyHealthPoint[]; className?: string}) {
  const colors = useChartColors()
  const hasData = data.length > 0 && data.some(d => d.flowCount > 0)

  return (
    <CardShell title="Health Score Trend · 30d" className={className}>
      {!hasData ? (
        <CardPlaceholder message="Health trend appears after you analyze flows across multiple sessions." />
      ) : (
        <div className="h-48">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={data} margin={{top: 5, right: 10, left: -20, bottom: 0}}>
              <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} vertical={false} />
              <ReferenceArea y1={80} y2={100} fill={colors.success} fillOpacity={0.05} />
              <ReferenceArea y1={50} y2={80} fill={colors.warning} fillOpacity={0.05} />
              <ReferenceArea y1={0} y2={50} fill={colors.error} fillOpacity={0.05} />
              <XAxis
                dataKey="date"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                tickFormatter={(d: string) => d.slice(5)}
                minTickGap={20}
              />
              <YAxis
                domain={[0, 100]}
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                width={36}
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12,
                }}
                labelStyle={{color: 'var(--text-primary)'}}
              />
              <Line
                type="monotone"
                dataKey="avgHealth"
                stroke={colors.brand500}
                strokeWidth={2}
                dot={false}
                isAnimationActive={false}
                connectNulls
              />
            </LineChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
