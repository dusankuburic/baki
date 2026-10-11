import {useTranslation} from 'react-i18next'
import clsx from 'clsx'

export function FilterChip({
  label,
  count,
  pressed,
  tone,
  disabled,
  ariaLabel,
  onClick,
}: {
  label: string
  count?: number
  pressed: boolean
  tone?: {bg: string; text: string}
  disabled?: boolean
  ariaLabel?: string
  onClick: () => void
}) {
  const {t} = useTranslation('dashboard')
  const on = pressed ? (tone ?? {bg: 'bg-brand-500/10', text: 'text-brand-400'}) : null
  return (
    <button
      type="button"
      aria-pressed={pressed}
      aria-label={ariaLabel ?? (count === undefined ? label : t('analytics.filters.chipAria', {label, count}))}
      disabled={disabled}
      onClick={onClick}
      className={clsx(
        'analytics-chip inline-flex h-7 shrink-0 items-center gap-1 whitespace-nowrap rounded-full border px-2.5 text-xs font-medium transition-colors duration-fast focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40',
        on
          ? `${on.bg} ${on.text} border-transparent`
          : 'border-border-subtle bg-transparent text-text-secondary hover:border-border-default hover:text-text-primary',
        disabled && 'cursor-not-allowed opacity-50',
      )}
    >
      <span>{label}</span>
      {count !== undefined && <span className="font-normal tabular-nums opacity-70">{count}</span>}
    </button>
  )
}
