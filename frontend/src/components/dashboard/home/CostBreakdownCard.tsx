import {PieChart, Pie, Cell, Tooltip, ResponsiveContainer} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {ProviderCost} from '@/types'

const PROVIDER_COLORS: Record<string, string> = {
  claude: '#d97757',
  anthropic: '#d97757',
  openai: '#10a37f',
  gemini: '#4285f4',
  copilot: '#6366f1',
  'github-models': '#8b5cf6',
  grok: '#1d9bf0',
  glm: '#3b82f6',
}

export function CostBreakdownCard({data, className}: {data: ProviderCost[]; className?: string}) {
  const colors = useChartColors()
  const hasData = data.length > 0 && data.some(d => d.cost > 0)
  const totalCost = data.reduce((s, d) => s + d.cost, 0)

  const chartData = data
    .filter(d => d.cost > 0)
    .map(d => ({
      name: d.provider,
      value: Math.round(d.cost * 100) / 100,
    }))

  return (
    <CardShell title="AI Cost by Provider" className={className}>
      {!hasData ? (
        <CardPlaceholder message="No AI costs recorded. Use the chat to start tracking spend." />
      ) : (
        <div className="h-48 flex items-center gap-2">
          <div className="flex-1 h-full relative">
            <ResponsiveContainer width="100%" height="100%">
              <PieChart>
                <Pie
                  data={chartData}
                  dataKey="value"
                  nameKey="name"
                  cx="50%"
                  cy="50%"
                  innerRadius="55%"
                  outerRadius="85%"
                  paddingAngle={2}
                  isAnimationActive={false}
                >
                  {chartData.map(entry => (
                    <Cell key={entry.name} fill={PROVIDER_COLORS[entry.name.toLowerCase()] ?? colors.brand500} />
                  ))}
                </Pie>
                <Tooltip
                  contentStyle={{
                    backgroundColor: 'var(--glass-bg)',
                    borderColor: 'var(--border-subtle)',
                    borderRadius: 8,
                    backdropFilter: 'var(--glass-blur)',
                    fontSize: 12,
                    fontVariantNumeric: 'tabular-nums',
                  }}
                  formatter={v => [`$${Number(v).toFixed(2)}`, 'Cost']}
                />
              </PieChart>
            </ResponsiveContainer>
            <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none">
              <span className="text-xl font-black font-mono tabular-nums text-text-primary">
                ${totalCost.toFixed(2)}
              </span>
              <span className="text-2xs text-text-tertiary">30d total</span>
            </div>
          </div>
          <div className="flex flex-col gap-1 min-w-0">
            {chartData.slice(0, 5).map(entry => (
              <div key={entry.name} className="flex items-center gap-1.5 text-2xs">
                <span
                  className="w-2.5 h-2.5 rounded-sm shrink-0"
                  style={{backgroundColor: PROVIDER_COLORS[entry.name.toLowerCase()] ?? colors.brand500}}
                />
                <span className="text-text-secondary truncate">{entry.name}</span>
                <span className="text-text-tertiary tabular-nums ml-auto">${entry.value.toFixed(2)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </CardShell>
  )
}
