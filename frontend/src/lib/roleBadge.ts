// Semantic-token badge classes for a user role. Uses the app's semantic color
// scale (not the flow-block palette) so the meaning reads consistently with
// every other status/severity badge in the app.
export function roleBadgeClass(role: string): string {
  switch (role) {
    case 'admin':
      return 'bg-brand-500/10 text-brand-400'
    case 'member':
      return 'bg-semantic-info/10 text-semantic-info'
    case 'viewer':
      return 'bg-surface-4 text-text-secondary'
    default:
      return 'bg-surface-4 text-text-tertiary'
  }
}
