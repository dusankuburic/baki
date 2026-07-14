import type {Severity} from '@/types'

interface Props {
  severity: Severity
  size?: 'sm' | 'md'
}

const config: Record<Severity, {bg: string; text: string; label: string}> = {
  error: {bg: 'bg-red-500/15', text: 'text-red-400', label: 'Error'},
  warning: {bg: 'bg-amber-500/15', text: 'text-amber-400', label: 'Warn'},
  info: {bg: 'bg-blue-500/15', text: 'text-blue-400', label: 'Info'},
}

export default function SeverityBadge({severity, size = 'sm'}: Props) {
  const c = config[severity]
  const sizeClass = size === 'sm' ? 'text-2xs px-1.5 py-0.5' : 'text-xs px-2 py-0.5'
  return (
    <span className={`inline-flex items-center rounded font-medium ${c.bg} ${c.text} ${sizeClass}`}>{c.label}</span>
  )
}
