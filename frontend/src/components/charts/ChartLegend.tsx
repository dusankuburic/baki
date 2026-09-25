import clsx from 'clsx'
import type {LegendItem} from './chartTypes'

// An HTML legend rendered under the plot, in text tokens with a swatch that
// mirrors the mark. A single series needs no legend — the card title names it.
export function ChartLegend({items, className}: {items: LegendItem[]; className?: string}) {
  if (items.length < 2) return null
  return (
    <ul className={clsx('chart-legend flex flex-wrap gap-x-4 gap-y-1 text-xs text-text-secondary', className)}>
      {items.map(item => (
        <li key={item.key} className="flex items-center gap-1.5">
          <LegendSwatch color={item.color} shape={item.shape ?? 'rect'} />
          <span>{item.label}</span>
        </li>
      ))}
    </ul>
  )
}

export function LegendSwatch({color, shape}: {color: string; shape: 'rect' | 'line' | 'dot'}) {
  if (shape === 'line') {
    return <span aria-hidden="true" className="inline-block h-0.5 w-3 rounded-full" style={{background: color}} />
  }
  if (shape === 'dot') {
    return <span aria-hidden="true" className="inline-block h-2 w-2 rounded-full" style={{background: color}} />
  }
  return <span aria-hidden="true" className="inline-block h-2.5 w-2.5 rounded-sm" style={{background: color}} />
}
