import {AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {DailyTokenUsage} from '@/types'

// AITokenUsageCard plots prompt vs completion tokens over the gap-filled 14-day
// window. Empty in local/desktop mode (no usage tracking) → placeholder.
export function AITokenUsageCard({data, className}: {data: DailyTokenUsage[]; className?: string}) {
  const colors = useChartColors()
  const hasUsage = data.length > 0 && data.some(d => d.tokensIn > 0 || d.tokensOut > 0)

  return (
    <CardShell title="AI Token Usage · 14d" className={className}>
      {data.length === 0 ? (
        <CardPlaceholder message="Token usage isn't tracked in desktop mode." />
      ) : !hasUsage ? (
        <CardPlaceholder message="No AI usage in the last 14 days." />
      ) : (
        <div className="h-48">
          <ResponsiveContainer width="100%" height="100%">
            <AreaChart data={data} margin={{top: 10, right: 10, left: -20, bottom: 0}}>
              <defs>
                <linearGradient id="dashTokColorIn" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={colors.brand400} stopOpacity={0.4} />
                  <stop offset="95%" stopColor={colors.brand400} stopOpacity={0} />
                </linearGradient>
                <linearGradient id="dashTokColorOut" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={colors.brand600} stopOpacity={0.4} />
                  <stop offset="95%" stopColor={colors.brand600} stopOpacity={0} />
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
                tickFormatter={compact}
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
              <Area type="monotone" dataKey="tokensIn" name="In" stackId="1" stroke={colors.brand400} fill="url(#dashTokColorIn)" isAnimationActive={false} />
              <Area type="monotone" dataKey="tokensOut" name="Out" stackId="1" stroke={colors.brand600} fill="url(#dashTokColorOut)" isAnimationActive={false} />
            </AreaChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}

function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(n)
}
