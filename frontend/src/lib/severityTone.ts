import type {Severity, TriageStatus} from '@/types'

// severityTone is THE single source (U2.1) mapping finding severity → tone
// classes, backed by SEMANTIC design tokens so findings follow the theme
// (19 palettes redefine --error/--warning/--info). Before this, findings
// colors were raw `red-400`/`amber-400`/`blue-400` redefined independently in
// five components — they silently desynced from every theme.
export interface Tone {
  /** icon/label color */
  text: string
  /** subtle chip fill */
  bg: string
  /** chip outline */
  border: string
  /** solid status dot */
  dot: string
  /** list-row left accent */
  bar: string
}

const severityTones: Record<Severity, Tone> = {
  error: {
    text: 'text-semantic-error',
    bg: 'bg-semantic-error/15',
    border: 'border-semantic-error/30',
    dot: 'bg-semantic-error',
    bar: 'border-l-semantic-error',
  },
  warning: {
    text: 'text-semantic-warning',
    bg: 'bg-semantic-warning/15',
    border: 'border-semantic-warning/30',
    dot: 'bg-semantic-warning',
    bar: 'border-l-semantic-warning',
  },
  info: {
    text: 'text-semantic-info',
    bg: 'bg-semantic-info/15',
    border: 'border-semantic-info/30',
    dot: 'bg-semantic-info',
    bar: 'border-l-semantic-info',
  },
}

export function severityTone(severity: Severity): Tone {
  return severityTones[severity] ?? severityTones.info
}

// Triage statuses are a workflow, not a severity: resolved reads as success,
// in-progress as "attention", acknowledged as info, suppressed as muted.
const triageTones: Record<TriageStatus, Tone> = {
  open: {
    text: 'text-text-secondary',
    bg: 'bg-surface-3',
    border: 'border-border-default',
    dot: 'bg-text-tertiary',
    bar: 'border-l-border-strong',
  },
  acknowledged: {
    text: 'text-semantic-info',
    bg: 'bg-semantic-info/10',
    border: 'border-semantic-info/30',
    dot: 'bg-semantic-info',
    bar: 'border-l-semantic-info',
  },
  in_progress: {
    text: 'text-semantic-warning',
    bg: 'bg-semantic-warning/10',
    border: 'border-semantic-warning/30',
    dot: 'bg-semantic-warning',
    bar: 'border-l-semantic-warning',
  },
  resolved: {
    text: 'text-semantic-success',
    bg: 'bg-semantic-success/10',
    border: 'border-semantic-success/30',
    dot: 'bg-semantic-success',
    bar: 'border-l-semantic-success',
  },
  suppressed: {
    text: 'text-text-tertiary',
    bg: 'bg-surface-3',
    border: 'border-border-subtle',
    dot: 'bg-text-disabled',
    bar: 'border-l-border-subtle',
  },
}

export function triageTone(status: TriageStatus): Tone {
  return triageTones[status] ?? triageTones.open
}

// The shared "this action writes/changes something good" affordance
// (Apply fix and friends) — previously raw emerald sprinkled per-component.
export const successActionTone = {
  text: 'text-semantic-success',
  hoverText: 'hover:text-semantic-success',
  bg: 'bg-semantic-success/10',
  border: 'border-semantic-success/30',
} as const
