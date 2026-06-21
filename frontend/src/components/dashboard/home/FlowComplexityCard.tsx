import {ScatterChart, Scatter, XAxis, YAxis, ZAxis, Tooltip, ResponsiveContainer, CartesianGrid, Cell} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors, healthColor} from './useChartColors'
import type {FlowComplexityPoint} from '@/types'

export function FlowComplexityCard({data, className}: {data: FlowComplexityPoint[]; className?: string}) {
  const colors = useChartColors()
  const hasData = data.length > 0 && data.some(d => d.blockCount > 0)

  const chartData = data.map(d => ({
    name: d.flowName,
    blocks: d.blockCount,
    findings: d.findingCount,
    health: d.healthScore,
  }))

  const dotColor = (health: number) => healthColor(health, colors)

  return (
    <CardShell title="Flow Complexity" className={className}>
      {!hasData ? (
        <CardPlaceholder message="Complexity scatter appears after analyzing flows with block metadata." />
      ) : (
        <div className="h-48">
          <ResponsiveContainer width="100%" height="100%">
            <ScatterChart margin={{top: 10, right: 10, left: -20, bottom: 0}}>
              <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} />
              <XAxis
                type="number"
                dataKey="blocks"
                name="Blocks"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                tickFormatter={(v: number) => String(v)}
              />
              <YAxis
                type="number"
                dataKey="findings"
                name="Findings"
                stroke={colors.borderStrong}
                tick={{fill: colors.textTertiary, fontSize: 11}}
                width={36}
              />
              <ZAxis type="number" dataKey="health" name="Health" domain={[0, 100]} range={[60, 400]} />
              <Tooltip
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12, fontVariantNumeric: 'tabular-nums',
                }}
                cursor={{strokeDasharray: '3 3', stroke: colors.textTertiary, strokeOpacity: 0.3}}
                formatter={(v, name) => {
                  if (name === 'Health') return [`${v}/100`, name]
                  return [v, name]
                }}
                labelFormatter={(_value, payload) => {
                  const item = payload[0]?.payload
                  return item ? item.name : ''
                }}
              />
              <Scatter data={chartData} isAnimationActive={false}>
                {chartData.map((entry, i) => (
                  <Cell key={`cell-${i}`} fill={dotColor(entry.health)} fillOpacity={0.6} strokeWidth={1} stroke={dotColor(entry.health)} />
                ))}
              </Scatter>
            </ScatterChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
