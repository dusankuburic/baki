import type {Severity} from '@/types'
import SeverityBadge from './SeverityBadge'

interface Props {
  errors: number
  warnings: number
  info: number
  filter: Set<Severity>
  onToggle: (s: Severity) => void
}

export default function SeverityFilter({errors, warnings, info, filter, onToggle}: Props) {
  const items: {sev: Severity; count: number}[] = [
    {sev: 'error', count: errors},
    {sev: 'warning', count: warnings},
    {sev: 'info', count: info},
  ]

  return (
    <div className="flex gap-1">
      {items.map(({sev, count}) => (
        <button
          key={sev}
          onClick={() => onToggle(sev)}
          className={`flex items-center gap-1 px-2 py-1 rounded text-xs transition-colors ${
            filter.has(sev)
              ? 'bg-surface-3 text-text-primary'
              : 'bg-surface-2 text-text-tertiary line-through opacity-50'
          }`}
        >
          <SeverityBadge severity={sev} />
          <span>{count}</span>
        </button>
      ))}
    </div>
  )
}
