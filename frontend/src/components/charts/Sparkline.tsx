import {useId} from 'react'
import {useChartTheme} from './useChartTheme'

// A 12-point trend under a stat tile: the history in the de-emphasis hue, the
// current period marked in the accent with a surface ring. Decorative — the
// tile's value and delta carry the information.
export function Sparkline({values, height = 28, className}: {values: number[]; height?: number; className?: string}) {
  const t = useChartTheme()
  const id = useId()
  const pts = values.filter(v => Number.isFinite(v))
  if (pts.length < 2) return null
  const width = 120
  const pad = 5
  const min = Math.min(...pts)
  const max = Math.max(...pts)
  const span = max - min || 1
  const x = (i: number) => pad + (i * (width - pad * 2)) / (pts.length - 1)
  const y = (v: number) => pad + ((max - v) / span) * (height - pad * 2)
  const d = pts.map((v, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(' ')
  const last = pts.length - 1
  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      width="100%"
      height={height}
      preserveAspectRatio="none"
      className={className}
      aria-hidden="true"
      focusable="false"
      data-sparkline={id}
    >
      <path
        d={d}
        fill="none"
        stroke={t.muted}
        strokeWidth={2}
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
      <circle cx={x(last)} cy={y(pts[last])} r={4} fill={t.emphasis} stroke={t.surface} strokeWidth={2} />
    </svg>
  )
}
