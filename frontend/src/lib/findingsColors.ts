export const categoryColors: Record<string, string> = {
  Security: 'text-red-400',
  Reliability: 'text-amber-400',
  Performance: 'text-orange-400',
  Style: 'text-purple-400',
  Logic: 'text-cyan-400',
}

export const categoryBackgrounds: Record<string, string> = {
  Security: 'bg-red-500/10',
  Reliability: 'bg-amber-500/10',
  Performance: 'bg-orange-500/10',
  Style: 'bg-purple-500/10',
  Logic: 'bg-cyan-500/10',
}

export const categoryBadgeClass = (category?: string): string => {
  if (!category) return 'text-text-tertiary bg-surface-3'
  return `${categoryBackgrounds[category] ?? 'bg-surface-3'} ${categoryColors[category] ?? 'text-text-tertiary'}`
}
