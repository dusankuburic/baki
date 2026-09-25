import {ScatterChart, Scatter, XAxis, YAxis, ZAxis, Tooltip, CartesianGrid, Cell} from 'recharts'
import {Zap, Target, Wrench, Clock} from 'lucide-react'
import {
  useChartTheme,
  useReducedMotion,
  ChartFigure,
  ChartTooltip,
  ChartLegend,
  gridProps,
  tickStyle,
  yAxisNumber,
  cursorLine,
  CHART_MARGIN,
} from '@/components/charts'
import type {Finding, SubflowMetrics} from '@/types'

// ComplexityScatter correlates each subflow's cyclomatic vs cognitive
// complexity (bubble size = max nesting depth). The two metrics measure
// different things — cyclomatic counts paths, cognitive counts how hard those
// paths are to read — so an outlier in the top-left (low cyclomatic, high
// cognitive) flags a deceptively simple-looking subflow that's a pain to
// maintain. Pure report data: no extra fetch.
export function ComplexityScatter({subflows}: {subflows: SubflowMetrics[]}) {
  const t = useChartTheme()
  const reduced = useReducedMotion()
  const data = subflows.map(s => ({
    name: s.subflowName,
    cyclo: s.cyclomaticComplexity,
    cog: s.cognitiveComplexity,
    depth: s.maxNestingDepth,
  }))

  if (subflows.length === 0) return null

  // Colour is state, not identity: cognitive load bands on the status tokens.
  const pointColor = (cog: number) => (cog > 30 ? t.status.error : cog > 15 ? t.status.warning : t.status.success)
  const legend = [
    {key: 'low', label: 'Cognitive ≤ 15', color: t.status.success, shape: 'dot' as const},
    {key: 'mid', label: 'Cognitive 16–30', color: t.status.warning, shape: 'dot' as const},
    {key: 'high', label: 'Cognitive > 30', color: t.status.error, shape: 'dot' as const},
  ]

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Complexity Map</h3>
      <p className="text-2xs text-text-tertiary mb-2">
        Cyclomatic vs cognitive per subflow · bubble = nesting depth. Top-left = deceptively hard to read.
      </p>
      <div
        role="group"
        aria-roledescription="chart"
        aria-label="Scatter plot of subflows by cyclomatic and cognitive complexity; bubble size shows nesting depth"
      >
        <ChartFigure height={232}>
          <ScatterChart margin={{...CHART_MARGIN, bottom: 16}}>
            <CartesianGrid {...gridProps(t)} vertical />
            <XAxis
              type="number"
              dataKey="cyclo"
              name="Cyclomatic"
              axisLine={{stroke: t.axis}}
              tickLine={false}
              tick={tickStyle(t)}
              tickMargin={8}
              allowDecimals={false}
              label={{value: 'Cyclomatic', position: 'insideBottom', offset: -12, fill: t.tick, fontSize: 10}}
            />
            <YAxis type="number" dataKey="cog" name="Cognitive" {...yAxisNumber(t, v => String(v))} />
            <ZAxis type="number" dataKey="depth" range={[40, 320]} name="Depth" />
            <Tooltip
              cursor={cursorLine(t)}
              content={
                <ChartTooltip theme={t} labelFormatter={(_label, payload) => String(payload[0]?.payload?.name ?? '')} />
              }
            />
            <Scatter data={data} isAnimationActive={!reduced}>
              {data.map((d, i) => (
                <Cell key={i} fill={pointColor(d.cog)} fillOpacity={0.8} stroke={t.surface} strokeWidth={2} />
              ))}
            </Scatter>
          </ScatterChart>
        </ChartFigure>
      </div>
      <ChartLegend items={legend} className="mt-2" />
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
      tone: 'text-semantic-success',
      ring: 'border-semantic-success/30 bg-semantic-success/5',
    },
    {
      icon: Target,
      label: 'Strategic',
      desc: 'Manual errors',
      count: strategic,
      tone: 'text-semantic-error',
      ring: 'border-semantic-error/30 bg-semantic-error/5',
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
            <div className={`text-xl font-semibold ${q.tone}`}>{q.count}</div>
            <div className="text-2xs text-text-tertiary mt-0.5">{q.desc}</div>
          </div>
        ))}
      </div>
    </div>
  )
}
