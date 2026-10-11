import type {ReactNode} from 'react'
import type {ChartTheme} from './chartTypes'
import {formatDateLong} from '@/lib/format'

export interface TooltipEntry {
  name?: string | number
  value?: number | string | ReadonlyArray<number | string>
  color?: string
  stroke?: string
  fill?: string
  dataKey?: string | number
  payload?: Record<string, unknown>
}

export interface ChartTooltipProps {
  active?: boolean
  payload?: ReadonlyArray<TooltipEntry>
  label?: string | number
  theme: ChartTheme
  labelFormatter?: (label: string | number, payload: ReadonlyArray<TooltipEntry>) => ReactNode
  valueFormatter?: (value: number, name: string, entry: TooltipEntry) => string
  hideEmpty?: boolean
}

function defaultLabel(label: string | number | undefined): ReactNode {
  if (label === undefined || label === null || label === '') return null
  const s = String(label)
  return /^\d{4}-\d{2}-\d{2}$/.test(s) ? formatDateLong(s) : s
}

export function ChartTooltip({
  active,
  payload,
  label,
  theme,
  labelFormatter,
  valueFormatter,
  hideEmpty = true,
}: ChartTooltipProps) {
  if (!active || !payload || payload.length === 0) return null
  const rows = payload.filter(p => !hideEmpty || (p.value !== null && p.value !== undefined))
  if (rows.length === 0) return null
  const header = labelFormatter ? labelFormatter(label ?? '', payload) : defaultLabel(label)
  return (
    <div
      className="chart-tooltip rounded-lg border px-3 py-2 text-xs shadow-md"
      style={{background: theme.tooltip.bg, borderColor: theme.tooltip.border, color: theme.text.primary}}
    >
      {header !== null && header !== '' && <div className="mb-1 text-text-secondary">{header}</div>}
      <ul className="space-y-0.5">
        {rows.map((p, i) => {
          const color = p.color ?? p.stroke ?? p.fill ?? theme.muted
          const name = String(p.name ?? p.dataKey ?? '')
          const raw = Array.isArray(p.value) ? p.value[0] : (p.value as number | string | undefined)
          const num = typeof raw === 'number' ? raw : Number(raw)
          const value = valueFormatter && Number.isFinite(num) ? valueFormatter(num, name, p) : String(raw ?? '')
          return (
            <li key={`${name}-${i}`} className="flex items-center gap-2">
              <span
                aria-hidden="true"
                className="inline-block h-0.5 w-3 shrink-0 rounded-full"
                style={{background: color}}
              />
              <span className="font-semibold tabular-nums text-text-primary">{value}</span>
              <span className="text-text-secondary">{name}</span>
            </li>
          )
        })}
      </ul>
    </div>
  )
}
