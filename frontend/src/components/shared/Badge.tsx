import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'
import {severityTone} from '@/lib/severityTone'

export type BadgeTone = 'neutral' | 'brand' | 'info' | 'success' | 'warning' | 'error'

export interface BadgeToneSpec {
  /** Tailwind classes: the fill, the outline and the text colour. */
  className: string
  /** The CSS custom property the fill is mixed from… */
  fill: string
  /** …and how much of it is laid over the surface (1 = solid). */
  fillAlpha: number
  /** The per-theme text colour token (index.css, every theme block). */
  fg: string
}

// Badge text cannot ride the tone colour itself: `text-brand-400` on a 15 %
// brand tint measured 1.2–2.5:1 in the light themes, and the semantic
// colours fail on their own tints in dozens of theme/surface pairs. Each tone
// therefore has a --badge-<tone>-fg token per theme, chosen to pass 4.5:1 on
// surface-1, surface-2 and the badge's own fill over either.
// lib/themeContrast.test.ts checks exactly these specs in every theme, so a
// new tone or a changed fill is measured, not eyeballed.
//
// The error/warning/info fills and outlines come from severityTone, so a
// badge and a finding chip of the same severity read as one colour.
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
  /** Leading icon, decorative (the text carries the meaning). */
  icon?: LucideIcon
  /** The visible text: a word or two. */
  children: React.ReactNode
  /**
   * What a screen reader hears instead of the visible text, for a badge
   * whose short text needs context ("SHARED" → "Shared by Contoso"). The
   * visible text is hidden from assistive technology then, so it is not
   * read twice.
   */
  srLabel?: string
  /** Hover hint (mouse only: put anything essential in srLabel as well). */
  title?: string
  /** Uppercase small caps, the app's badge style. Off for sentence-case chips. */
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
        // relative: contains the sr-only text, which is absolutely positioned
        // and would otherwise sit against the nearest positioned ancestor.
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
