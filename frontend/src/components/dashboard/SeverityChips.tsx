import clsx from 'clsx'
import {severityTone} from '@/lib/severityTone'
import {categoryBadgeClass} from '@/lib/findingsColors'

const chipOf = (severity: 'error' | 'warning' | 'info'): string => {
  const tone = severityTone(severity)
  return `${tone.text} ${tone.bg}`
}

export const sevChip: Record<string, string> = {
  error: chipOf('error'),
  warning: chipOf('warning'),
  info: chipOf('info'),
}

export function SeverityChips({
  bySeverity,
  byCategory,
}: {
  bySeverity: Record<string, number>
  byCategory: Record<string, number>
}) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {Object.entries(bySeverity).map(([sev, n]) => (
        <span
          key={sev}
          className={clsx(
            'text-sm font-bold px-2 py-0.5 rounded-full',
            sevChip[sev] ?? 'text-text-tertiary bg-surface-3',
          )}
        >
          {n} {sev}
        </span>
      ))}
      {Object.entries(byCategory).map(([cat, n]) => (
        <span
          key={cat}
          className={clsx('text-sm font-bold px-2 py-0.5 rounded-full', categoryBadgeClass(cat))}
        >
          {n} {cat}
        </span>
      ))}
    </div>
  )
}
