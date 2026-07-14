import {PieChart, Pie, Cell, Tooltip, ResponsiveContainer} from 'recharts'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'

// Confidence tiers → fixed semantic colors so the donut reads the same across
// themes. High = trust it, medium = review, low = likely false positive.
const CONF_COLORS: Record<string, {color: string; label: string}> = {
  high: {color: '#22c55e', label: 'High'},
  medium: {color: '#eab308', label: 'Medium'},
  low: {color: '#71717a', label: 'Low'},
}

interface Slice {
  key: string
  value: number
}

// ConfidenceDonutCard shows how much to trust the current findings: the share
// that is high-confidence vs heuristic/low. Populated in both modes (local
// derives it from the session cache). Renders a placeholder when there are no
// findings rather than a misleading full-grey donut.
export function ConfidenceDonutCard({confidence, className}: {confidence: Record<string, number>; className?: string}) {
  const colors = useChartColors()
  const conf = confidence ?? {}
  const slices: Slice[] = (['high', 'medium', 'low'] as const)
    .map(k => ({key: k, value: conf[k] ?? 0}))
    .filter(s => s.value > 0)
  const total = slices.reduce((s, x) => s + x.value, 0)

  return (
    <CardShell title="Finding Confidence" className={className}>
      {total === 0 ? (
        <CardPlaceholder message="Analyze a flow to see how much to trust its findings." />
      ) : (
        <div className="h-48 flex items-center gap-2">
          <div
            className="flex-1 h-full relative"
            role="img"
            aria-label="Donut chart of findings by confidence tier: high, medium, and low"
          >
            <ResponsiveContainer width="100%" height="100%">
              <PieChart>
                <Pie
                  data={slices}
                  dataKey="value"
                  nameKey="key"
                  cx="50%"
                  cy="50%"
                  innerRadius="55%"
                  outerRadius="85%"
                  paddingAngle={2}
                  isAnimationActive={false}
                >
                  {slices.map(s => (
                    <Cell key={s.key} fill={CONF_COLORS[s.key]?.color ?? colors.brand500} />
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
                  formatter={(v, _name, entry) => {
                    const tier = CONF_COLORS[(entry?.payload as Slice | undefined)?.key ?? '']
                    const pct = total > 0 ? Math.round((Number(v) / total) * 100) : 0
                    return [`${v} (${pct}%)`, tier?.label ?? '']
                  }}
                />
              </PieChart>
            </ResponsiveContainer>
            <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none">
              <span className="text-xl font-black font-mono tabular-nums text-text-primary">{total}</span>
              <span className="text-2xs text-text-tertiary">findings</span>
            </div>
          </div>
          <div className="flex flex-col gap-1 min-w-0">
            {slices.map(s => (
              <div key={s.key} className="flex items-center gap-1.5 text-2xs">
                <span
                  className="w-2.5 h-2.5 rounded-sm shrink-0"
                  style={{backgroundColor: CONF_COLORS[s.key]?.color ?? colors.brand500}}
                />
                <span className="text-text-secondary">{CONF_COLORS[s.key]?.label ?? s.key}</span>
                <span className="text-text-tertiary tabular-nums ml-auto">{Math.round((s.value / total) * 100)}%</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </CardShell>
  )
}
