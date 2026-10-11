
export type HealthTier = 'good' | 'fair' | 'poor' | 'critical'

export interface HealthTierInfo {
  tier: HealthTier
  label: string
  text: string
  bg: string
  border: string
  fill: string
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
