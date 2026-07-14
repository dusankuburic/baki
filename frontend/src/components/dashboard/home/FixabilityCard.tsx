import {RadialBarChart, RadialBar, PolarAngleAxis, ResponsiveContainer} from 'recharts'
import {Wrench, Check} from 'lucide-react'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {Fixability} from '@/types'

// FixabilityCard answers "how much of my debt is cheaply fixable?": the share
// of findings that carry a one-click deterministic fix (ring), plus the catalog
// fact "A of B rules ship a fixer" so the user knows the ceiling. Populated in
// both modes.
export function FixabilityCard({data, className}: {data: Fixability; className?: string}) {
  const colors = useChartColors()
  const {available = 0, total = 0, autoFixableRules = 0, totalRules = 0} = data ?? {}
  const pct = total > 0 ? Math.round((available / total) * 100) : 0
  const ring = [{name: 'fixable', value: pct}]
  const catalogPct = totalRules > 0 ? Math.round((autoFixableRules / totalRules) * 100) : 0

  return (
    <CardShell title="Fix Availability" className={className}>
      {total === 0 ? (
        // No findings yet — but the catalog ceiling ("A of B rules ship a
        // fixer") is static and always known, so keep that fact visible.
        <div className="flex flex-col h-full">
          <CardPlaceholder message="Analyze a flow to see how many findings are one-click fixable." />
          {totalRules > 0 && (
            <div className="flex items-center gap-2 rounded-lg bg-surface-3/50 px-2.5 py-2 mt-2">
              <Check size={15} className="text-brand-400 shrink-0" />
              <div className="min-w-0">
                <div className="text-sm font-mono tabular-nums text-text-primary leading-tight">
                  {autoFixableRules}/{totalRules}
                </div>
                <div className="text-2xs text-text-tertiary leading-tight">rules ship a fixer · {catalogPct}%</div>
              </div>
            </div>
          )}
        </div>
      ) : (
        <div className="flex flex-col h-full">
          <div className="relative h-36" role="img" aria-label={`Gauge showing ${pct}% of findings are auto-fixable`}>
            <ResponsiveContainer width="100%" height="100%">
              <RadialBarChart innerRadius="70%" outerRadius="100%" data={ring} startAngle={180} endAngle={0}>
                <PolarAngleAxis type="number" domain={[0, 100]} angleAxisId={0} tick={false} />
                <RadialBar
                  background={{fill: colors.surface3}}
                  dataKey="value"
                  cornerRadius={10}
                  fill={colors.success}
                  angleAxisId={0}
                  isAnimationActive={false}
                />
              </RadialBarChart>
            </ResponsiveContainer>
            <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none">
              <span className="text-3xl font-black font-mono tabular-nums text-text-primary">{pct}%</span>
              <span className="text-2xs uppercase tracking-widest text-text-tertiary mt-0.5">auto-fixable</span>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-2 mt-2">
            <div className="flex items-center gap-2 rounded-lg bg-surface-3/50 px-2.5 py-2">
              <Wrench size={15} className="text-success shrink-0" />
              <div className="min-w-0">
                <div className="text-sm font-mono tabular-nums text-text-primary leading-tight">
                  {available}/{total}
                </div>
                <div className="text-2xs text-text-tertiary leading-tight">findings fixable</div>
              </div>
            </div>
            <div className="flex items-center gap-2 rounded-lg bg-surface-3/50 px-2.5 py-2">
              <Check size={15} className="text-brand-400 shrink-0" />
              <div className="min-w-0">
                <div className="text-sm font-mono tabular-nums text-text-primary leading-tight">
                  {autoFixableRules}/{totalRules}
                </div>
                <div className="text-2xs text-text-tertiary leading-tight">rules · {catalogPct}%</div>
              </div>
            </div>
          </div>
        </div>
      )}
    </CardShell>
  )
}
