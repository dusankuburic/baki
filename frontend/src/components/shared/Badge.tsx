import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'
import {severityTone} from '@/lib/severityTone'

export type BadgeTone = 'neutral' | 'brand' | 'info' | 'success' | 'warning' | 'error'

export interface BadgeToneSpec {
  className: string
  fill: string
  fillAlpha: number
  fg: string
}

export const BADGE_TONES: Record<BadgeTone, BadgeToneSpec> = {
  neutral: {
    className: 'bg-surface-3 border-border-default text-(--badge-neutral-fg)',
    fill: '--surface-3',
    fillAlpha: 1,
    fg: '--badge-neutral-fg',
  },
  brand: {
    className: 'bg-brand-500/15 border-brand-500/30 text-(--badge-brand-fg)',
    fill: '--brand-500',
    fillAlpha: 0.15,
    fg: '--badge-brand-fg',
  },
  info: {
    className: clsx(severityTone('info').bg, severityTone('info').border, 'text-(--badge-info-fg)'),
    fill: '--info',
    fillAlpha: 0.15,
    fg: '--badge-info-fg',
  },
  success: {
    className: 'bg-semantic-success/15 border-semantic-success/30 text-(--badge-success-fg)',
    fill: '--success',
    fillAlpha: 0.15,
    fg: '--badge-success-fg',
  },
  warning: {
    className: clsx(severityTone('warning').bg, severityTone('warning').border, 'text-(--badge-warning-fg)'),
    fill: '--warning',
    fillAlpha: 0.15,
    fg: '--badge-warning-fg',
  },
  error: {
    className: clsx(severityTone('error').bg, severityTone('error').border, 'text-(--badge-error-fg)'),
    fill: '--error',
    fillAlpha: 0.15,
    fg: '--badge-error-fg',
  },
}

type BadgeProps = {
  tone?: BadgeTone
  icon?: LucideIcon
  children: React.ReactNode
  srLabel?: string
  title?: string
  caps?: boolean
  className?: string
}

export default function Badge({
  tone = 'neutral',
  icon: Icon,
  children,
  srLabel,
  title,
  caps = true,
  className,
}: BadgeProps) {
  return (
    <span
      title={title}
      className={clsx(
        'relative inline-flex items-center gap-1 shrink-0 max-w-full rounded border px-1.5 py-px text-2xs font-semibold whitespace-nowrap',
        caps && 'uppercase tracking-wider',
        BADGE_TONES[tone].className,
        className,
      )}
    >
      {Icon && <Icon size={10} aria-hidden="true" className="shrink-0" />}
      {srLabel ? (
        <>
          <span aria-hidden="true" className="truncate">
            {children}
          </span>
          <span className="sr-only">{srLabel}</span>
        </>
      ) : (
        <span className="truncate">{children}</span>
      )}
    </span>
  )
}
