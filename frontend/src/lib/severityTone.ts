import {TRIAGE_STATUSES, isKnown, type Severity, type Tolerant, type TriageStatus} from '@/types'

export interface Tone {
  text: string
  bg: string
  border: string
  dot: string
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

export function triageTone(status: Tolerant<TriageStatus>): Tone {
  return isKnown(TRIAGE_STATUSES, status) ? triageTones[status] : triageTones.open
}

export const successActionTone = {
  text: 'text-semantic-success',
  hoverText: 'hover:text-semantic-success',
  bg: 'bg-semantic-success/10',
  border: 'border-semantic-success/30',
} as const
