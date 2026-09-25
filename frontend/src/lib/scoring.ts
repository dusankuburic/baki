// Health score → tier. THE single source for every health colour in the app
// (KPI tiles, charts, library rows, the inspector bar, shared reports): the
// boundaries are 80/60/40 and the colours are the semantic tokens, so all 19
// themes retint them together. Poor and critical share the error token — the
// app has three status colours — and carry their distinction in the label.

export type HealthTier = 'good' | 'fair' | 'poor' | 'critical'

export interface HealthTierInfo {
  tier: HealthTier
  label: string
  /** text colour class */
  text: string
  /** subtle fill class */
  bg: string
  /** outline class */
  border: string
  /** solid fill class (bars, dots) */
  fill: string
  /** CSS custom property carrying the colour, for inline styles and SVG */
  token: '--success' | '--warning' | '--error'
}

const TIERS: Record<HealthTier, HealthTierInfo> = {
  good: {
    tier: 'good',
    label: 'Good',
    text: 'text-semantic-success',
    bg: 'bg-semantic-success/10',
    border: 'border-semantic-success/20',
    fill: 'bg-semantic-success',
    token: '--success',
  },
  fair: {
    tier: 'fair',
    label: 'Fair',
    text: 'text-semantic-warning',
    bg: 'bg-semantic-warning/10',
    border: 'border-semantic-warning/20',
    fill: 'bg-semantic-warning',
    token: '--warning',
  },
  poor: {
    tier: 'poor',
    label: 'Poor',
    text: 'text-semantic-error',
    bg: 'bg-semantic-error/10',
    border: 'border-semantic-error/20',
    fill: 'bg-semantic-error',
    token: '--error',
  },
  critical: {
    tier: 'critical',
    label: 'Critical',
    text: 'text-semantic-error',
    bg: 'bg-semantic-error/10',
    border: 'border-semantic-error/20',
    fill: 'bg-semantic-error',
    token: '--error',
  },
}

export function healthTier(score: number): HealthTierInfo {
  if (score >= 80) return TIERS.good
  if (score >= 60) return TIERS.fair
  if (score >= 40) return TIERS.poor
  return TIERS.critical
}

export function scoreColor(score: number): string {
  return healthTier(score).text
}

export function scoreBg(score: number): string {
  const t = healthTier(score)
  return `${t.bg} ${t.border}`
}

export function scoreLabel(score: number): string {
  return healthTier(score).label
}
