import {useMemo} from 'react'
import {RadialBarChart, RadialBar, PolarAngleAxis, ResponsiveContainer} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors, healthColor} from './useChartColors'
import type {DashboardOverview} from '@/types'

// HealthGaugeCard shows the average flow-health score as a semicircular gauge.
// When no flow has been analyzed yet, it renders a placeholder rather than a
// misleading 0%.
export function HealthGaugeCard({overview, bySeverity, className}: {overview: DashboardOverview; bySeverity?: Record<string, number>; className?: string}) {
  const colors = useChartColors()
  const score = Math.max(0, Math.min(100, overview.avgHealthScore))
  const fill = healthColor(score, colors)
  const data = useMemo(() => [{name: 'health', value: score}], [score])

  const errors = bySeverity?.error ?? 0
  const warnings = bySeverity?.warning ?? 0
  const info = bySeverity?.info ?? 0
  // Surface why the score floored: a lone "0" reads as a broken widget without
  // the breakdown. Only shown when the score is actually zero AND there's data
  // to explain it.
  const zeroCause = score === 0 && (errors + warnings + info) > 0
    ? [errors && `${errors} errors`, warnings && `${warnings} warnings`, info && `${info} info`].filter(Boolean).join(' · ')
    : null

  return (
    <CardShell title="Avg Health" className={className}>
      {!overview.healthAvailable ? (
        <CardPlaceholder message="Analyze a flow to see its health score." />
      ) : (
        <div className="relative h-48">
          <ResponsiveContainer width="100%" height="100%">
            <RadialBarChart
              innerRadius="70%"
              outerRadius="100%"
              data={data}
              startAngle={180}
              endAngle={0}
            >
              <PolarAngleAxis type="number" domain={[0, 100]} angleAxisId={0} tick={false} />
              <RadialBar
                background={{fill: colors.surface3}}
                dataKey="value"
                cornerRadius={10}
                fill={fill}
                angleAxisId={0}
                isAnimationActive={false}
              />
            </RadialBarChart>
          </ResponsiveContainer>
          {/* Centered numeric label overlaid on the gauge. */}
          <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none">
            <span className="text-4xl font-black font-mono tabular-nums" style={{color: fill}}>{score}</span>
            <span className="text-2xs uppercase tracking-widest text-text-tertiary mt-1">of 100</span>
            {zeroCause && (
              <span className="text-2xs text-text-tertiary mt-1">{zeroCause}</span>
            )}
          </div>
        </div>
      )}
    </CardShell>
  )
}
