import type {ReactNode} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ArrowDownRight, ArrowUpRight, Minus, type LucideIcon} from 'lucide-react'
import {formatDelta} from '@/lib/format'
import {Sparkline} from './Sparkline'
import {Meter} from './Meter'

export interface StatDelta {
  current: number
  prev: number | null
  /** Which direction is good news here: findings down, health up, spend neutral. */
  goodWhen: 'up' | 'down' | 'neutral'
  /** "vs last 30 days" */
  periodLabel: string
}

// A single number as a figure: label, value (proportional figures, never
// mono), an optional signed delta coloured by direction × whether up is good,
// an optional 12-point sparkline, an optional meter. `hero` is the one number a
// view leads with. When `unavailable` is set the tile says so instead of a 0.
export function StatTile({
  label,
  value,
  sub,
  tone,
  icon: Icon,
  delta,
  spark,
  meter,
  hero = false,
  unavailable,
  onClick,
  className,
  children,
}: {
  label: string
  value: string
  sub?: string
  /** text colour class for the value (e.g. healthTier(score).text) */
  tone?: string
  icon?: LucideIcon
  delta?: StatDelta
  spark?: number[]
  meter?: {value: number; max: number; token?: string; label: string}
  hero?: boolean
  unavailable?: string
  onClick?: () => void
  className?: string
  children?: ReactNode
}) {
  const {t} = useTranslation('dashboard')
  const d = delta ? formatDelta(delta.current, delta.prev) : null
  const deltaTone =
    !d || d.direction === 'none'
      ? ''
      : d.direction === 'flat' || delta?.goodWhen === 'neutral'
        ? 'text-text-tertiary'
        : (d.direction === 'up') === (delta?.goodWhen === 'up')
          ? 'text-semantic-success'
          : 'text-semantic-error'
  const DeltaIcon = d?.direction === 'up' ? ArrowUpRight : d?.direction === 'down' ? ArrowDownRight : Minus
  const deltaText =
    d && d.direction !== 'none' && delta
      ? d.direction === 'flat'
        ? t('tile.deltaFlat', {period: delta.periodLabel})
        : d.direction === 'up'
          ? t('tile.deltaUp', {delta: d.text, period: delta.periodLabel})
          : t('tile.deltaDown', {delta: d.text, period: delta.periodLabel})
      : ''
  const shownValue = unavailable ? '—' : value
  const ariaLabel = [label, shownValue, unavailable ?? sub, deltaText].filter(Boolean).join(', ')
  const Tag = onClick ? 'button' : 'div'

  return (
    <Tag
      type={onClick ? 'button' : undefined}
      onClick={onClick}
      role={onClick ? undefined : 'group'}
      aria-label={ariaLabel}
      className={clsx(
        'stat-tile flex min-w-0 flex-col gap-1 rounded-xl border border-border-subtle bg-surface-2 p-4 text-left',
        onClick &&
          'cursor-pointer transition-colors duration-fast hover:border-border-strong hover:bg-surface-3 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40',
        className,
      )}
    >
      <span className="flex items-start gap-1.5 text-2xs uppercase leading-tight tracking-wider text-text-tertiary">
        {Icon && <Icon size={12} aria-hidden="true" className="mt-px shrink-0" />}
        <span>{label}</span>
      </span>
      <span
        className={clsx(
          'font-semibold leading-none',
          hero ? 'text-[48px]' : 'text-2xl',
          unavailable ? 'text-text-tertiary' : (tone ?? 'text-text-primary'),
        )}
      >
        {shownValue}
      </span>
      {(unavailable || sub) && <span className="text-2xs text-text-tertiary">{unavailable ?? sub}</span>}
      {deltaText && (
        <span className={clsx('flex items-center gap-1 text-2xs', deltaTone)}>
          <DeltaIcon size={12} aria-hidden="true" />
          {deltaText}
        </span>
      )}
      {spark && spark.length >= 2 && <Sparkline values={spark} className="mt-1" />}
      {meter && <Meter value={meter.value} max={meter.max} token={meter.token} label={meter.label} className="mt-1" />}
      {children}
    </Tag>
  )
}
