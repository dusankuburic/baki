import {BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, Cell} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {RuleFrequency} from '@/types'

// Rule ID → severity mapping for color coding
const RULE_SEVERITY: Record<string, string> = {
  'hardcoded-credential': 'error',
  'sensitive-exposure': 'error',
  'sql-injection-risk': 'error',
  'infinite-loop-risk': 'error',
  'unhandled-error': 'warning',
  'missing-timeout': 'warning',
  'empty-handler': 'warning',
  'error-swallow': 'warning',
  'slow-pattern': 'warning',
  'resource-leak': 'warning',
  'uninitialized-variable': 'warning',
  'file-op-no-error-handler': 'warning',
  'goto-antipattern': 'warning',
  'subflow-mismatch': 'warning',
}

export function RuleFrequencyCard({data, className}: {data: RuleFrequency[]; className?: string}) {
  const colors = useChartColors()
  const hasData = data.length > 0

  const chartData = data.slice(0, 10).map(d => ({
    rule: d.rule,
    count: d.count,
    sev: RULE_SEVERITY[d.rule] ?? 'info',
  }))

  const barColor = (sev: string) => {
    if (sev === 'error') return colors.error
    if (sev === 'warning') return colors.warning
    return colors.brand500
  }

  return (
    <CardShell title="Findings by Rule · Top 10" className={className}>
      {!hasData ? (
        <CardPlaceholder message="No rule frequency data yet. Analyze flows to populate this." />
      ) : (
        <div className="h-56">
          <ResponsiveContainer width="100%" height="100%">
            <BarChart data={chartData} layout="vertical" margin={{top: 0, right: 10, left: 0, bottom: 0}}>
              <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} horizontal={false} />
              <XAxis
                type="number"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                tickFormatter={(v: number) => String(v)}
              />
              <YAxis
                type="category"
                dataKey="rule"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 10}}
                width={120}
                tickFormatter={(d: string) => d.length > 18 ? d.slice(0, 17) + '…' : d}
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12,
                }}
                cursor={{fill: colors.surface3, fillOpacity: 0.3}}
              />
              <Bar dataKey="count" radius={[0, 4, 4, 0]} isAnimationActive={false}>
                {chartData.map((entry) => (
                  <Cell key={entry.rule} fill={barColor(entry.sev)} />
                ))}
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
