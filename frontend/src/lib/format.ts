// Shared number formatting.
// - `formatCompact` abbreviates large telemetry (token counts) to "5.3k" / "1.2M".
// - `formatCount` renders discrete counts with locale thousands separators ("1,234").

export function formatCompact(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

export function formatCount(n: number): string {
  return n.toLocaleString()
}
