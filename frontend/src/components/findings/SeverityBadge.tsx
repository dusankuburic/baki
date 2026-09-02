import {useTranslation} from 'react-i18next'
import type {Severity} from '@/types'
import {severityTone} from '@/lib/severityTone'

interface Props {
  severity: Severity
  size?: 'sm' | 'md'
}

// Tone classes come from the shared severityTone map (semantic tokens that
// follow the active theme) — see lib/severityTone.ts.

export default function SeverityBadge({severity, size = 'sm'}: Props) {
  const {t} = useTranslation('findings')
  // Direct literal calls (not a key variable) so the typed t() overloads
  // resolve per-key; the union-typed lookup confuses overload resolution.
  const labels = {error: t('badge.error'), warning: t('badge.warning'), info: t('badge.info')} as const
  const c = severityTone(severity)
  const sizeClass = size === 'sm' ? 'text-2xs px-1.5 py-0.5' : 'text-xs px-2 py-0.5'
  return (
    <span className={`inline-flex items-center rounded font-medium ${c.bg} ${c.text} ${sizeClass}`}>
      {labels[severity]}
    </span>
  )
}
