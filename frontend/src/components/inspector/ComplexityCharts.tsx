import {ScatterChart, Scatter, XAxis, YAxis, ZAxis, Tooltip, ResponsiveContainer, CartesianGrid, Cell} from 'recharts'
import {Zap, Target, Wrench, Clock} from 'lucide-react'
import {useChartColors} from '../dashboard/home/useChartColors'
import type {Finding, SubflowMetrics} from '@/types'

// ComplexityScatter correlates each subflow's cyclomatic vs cognitive
// complexity (bubble size = max nesting depth). The two metrics measure
// different things — cyclomatic counts paths, cognitive counts how hard those
// paths are to read — so an outlier in the top-left (low cyclomatic, high
// cognitive) flags a deceptively simple-looking subflow that's a pain to
// maintain. Pure report data: no extra fetch.
export function ComplexityScatter({subflows}: {subflows: SubflowMetrics[]}) {
  const colors = useChartColors()
  const data = subflows.map(s => ({
    name: s.subflowName,
    cyclo: s.cyclomaticComplexity,
    cog: s.cognitiveComplexity,
    depth: s.maxNestingDepth,
  }))

  if (subflows.length === 0) return null

  // Color each point by cognitive load so hotspots pop regardless of position.
  const pointColor = (cog: number) => (cog > 30 ? colors.error : cog > 15 ? colors.warning : colors.success)

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Complexity Map</h3>
      <p className="text-2xs text-text-tertiary mb-2">
        Cyclomatic vs cognitive per subflow · bubble = nesting depth. Top-left = deceptively hard to read.
      </p>
      <div
        className="h-56"
        role="img"
        aria-label="Scatter plot of subflows by cyclomatic and cognitive complexity; bubble size shows nesting depth"
      >
        <ResponsiveContainer width="100%" height="100%">
          <ScatterChart margin={{top: 10, right: 16, left: -16, bottom: 4}}>
            <CartesianGrid strokeDasharray="3 3" stroke={colors.borderStrong} strokeOpacity={0.3} />
            <XAxis
              type="number"
              dataKey="cyclo"
              name="Cyclomatic"
              stroke={colors.borderStrong}
              tick={{fill: colors.textTertiary, fontSize: 11}}
              label={{
                value: 'Cyclomatic',
                position: 'insideBottom',
                offset: -2,
                fill: colors.textTertiary,
                fontSize: 10,
              }}
            />
            <YAxis
              type="number"
              dataKey="cog"
              name="Cognitive"
              stroke={colors.borderStrong}
              tick={{fill: colors.textTertiary, fontSize: 11}}
              width={36}
            />
            <ZAxis type="number" dataKey="depth" range={[40, 320]} name="Depth" />
            <Tooltip
              cursor={{strokeDasharray: '3 3', stroke: colors.borderStrong}}
              contentStyle={{
                backgroundColor: 'var(--glass-bg)',
                borderColor: 'var(--border-subtle)',
                borderRadius: 8,
                backdropFilter: 'var(--glass-blur)',
                fontSize: 12,
                fontVariantNumeric: 'tabular-nums',
              }}
              formatter={(_v, _name, entry) => {
                const p = entry?.payload as {name: string; cyclo: number; cog: number; depth: number} | undefined
                return p ? [`${p.cyclo} cyc / ${p.cog} cog / depth ${p.depth}`, p.name] : []
              }}
            />
            <Scatter data={data} isAnimationActive={false}>
              {data.map((d, i) => (
                <Cell key={i} fill={pointColor(d.cog)} fillOpacity={0.7} />
              ))}
            </Scatter>
          </ScatterChart>
        </ResponsiveContainer>
      </div>
    </div>
  )
}

// ImpactEffortMatrix sorts the flow's findings into a 2×2 priority grid:
// severity (error vs warning/info) × effort (one-click fix vs manual). This is
// the "what should I fix first?" view — Quick Wins (auto-fixable errors) are
// the highest-leverage work. Pure report data: no extra fetch.
export function ImpactEffortMatrix({findings}: {findings: Finding[]}) {
  if (findings.length === 0) return null

  let quickWins = 0 // auto-fixable + error
  let strategic = 0 // manual + error
  let easyCleanup = 0 // auto-fixable + warning/info
  let backlog = 0 // manual + warning/info
  for (const f of findings) {
    const isError = f.severity === 'error'
    const fixable = !!f.autoFix
    if (isError && fixable) quickWins++
    else if (isError) strategic++
    else if (fixable) easyCleanup++
    else backlog++
  }

  const quadrants = [
    {
      icon: Zap,
      label: 'Quick Wins',
      desc: 'Auto-fixable errors',
      count: quickWins,
      tone: 'text-emerald-400',
      ring: 'border-emerald-500/30 bg-emerald-500/5',
    },
    {
      icon: Target,
      label: 'Strategic',
      desc: 'Manual errors',
      count: strategic,
      tone: 'text-red-400',
      ring: 'border-red-500/30 bg-red-500/5',
    },
    {
      icon: Wrench,
      label: 'Easy Cleanup',
      desc: 'Auto-fixable lower sev',
      count: easyCleanup,
      tone: 'text-brand-400',
      ring: 'border-brand-500/30 bg-brand-500/5',
    },
    {
      icon: Clock,
      label: 'Backlog',
      desc: 'Manual lower sev',
      count: backlog,
      tone: 'text-text-tertiary',
      ring: 'border-border-subtle bg-surface-0',
    },
  ]

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Impact × Effort</h3>
      <p className="text-2xs text-text-tertiary mb-2">Where to focus: Quick Wins are the highest-leverage fixes.</p>
      <div className="grid grid-cols-2 gap-2">
        {quadrants.map(q => (
          <div key={q.label} className={`p-3 rounded-lg border ${q.ring}`}>
            <div className="flex items-center gap-1.5 mb-1">
              <q.icon size={13} className={q.tone} />
              <span className="text-xs font-semibold text-text-primary">{q.label}</span>
            </div>
            <div className={`text-xl font-black font-mono tabular-nums ${q.tone}`}>{q.count}</div>
            <div className="text-2xs text-text-tertiary mt-0.5">{q.desc}</div>
          </div>
        ))}
      </div>
    </div>
  )
}
