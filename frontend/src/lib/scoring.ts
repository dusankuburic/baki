// Health-score → tailwind color utilities shared by MetricsTab,
// FindingsSummary, and AnalyticsDashboard.

export function scoreColor(score: number): string {
  if (score >= 80) return 'text-green-400'
  if (score >= 60) return 'text-amber-400'
  if (score >= 40) return 'text-orange-400'
  return 'text-red-400'
}

export function scoreBg(score: number): string {
  if (score >= 80) return 'bg-green-500/10 border-green-500/20'
  if (score >= 60) return 'bg-amber-500/10 border-amber-500/20'
  if (score >= 40) return 'bg-orange-500/10 border-orange-500/20'
  return 'bg-red-500/10 border-red-500/20'
}

export function scoreLabel(score: number): string {
  if (score >= 80) return 'Good'
  if (score >= 60) return 'Fair'
  if (score >= 40) return 'Poor'
  return 'Critical'
}
