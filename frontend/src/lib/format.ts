// Shared number formatting.
// - `formatCompact` abbreviates large telemetry (token counts) to "5.3k" / "1.2M".
// - `formatCount` renders discrete counts with locale thousands separators ("1,234").
// - The Intl helpers below serve the dashboards. They take an explicit locale so
//   tests can pin 'en-US' output, and they read a bare YYYY-MM-DD as a LOCAL day:
//   `new Date('2026-09-24')` is UTC midnight, which is the evening before west
//   of Greenwich and would shift every axis tick by a day there.

export function formatCompact(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

export function formatCount(n: number): string {
  return n.toLocaleString()
}

// Axis ticks and stat tiles: "1.2K", "3.4M" — Intl decides the suffix per locale.
export function formatCompactIntl(n: number, locale?: string): string {
  return new Intl.NumberFormat(locale, {notation: 'compact', maximumFractionDigits: 1}).format(n)
}

// AI spend is fractions of a dollar per call: sub-cent totals keep four
// decimals so "$0.0042" does not round to a misleading "$0.00".
export function formatCurrency(n: number, locale?: string, currency = 'USD'): string {
  const subCent = n !== 0 && Math.abs(n) < 0.01
  return new Intl.NumberFormat(locale, {
    style: 'currency',
    currency,
    minimumFractionDigits: 2,
    maximumFractionDigits: subCent ? 4 : 2,
  }).format(n)
}

export function formatPercent(ratio: number, digits = 0, locale?: string): string {
  return new Intl.NumberFormat(locale, {style: 'percent', maximumFractionDigits: digits}).format(ratio)
}

// parseDay turns a YYYY-MM-DD string into a local Date (or any other ISO
// string into a Date); null when it cannot be read.
export function parseDay(value: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  const d = m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : new Date(value)
  return Number.isNaN(d.getTime()) ? null : d
}

// "Sep 24" — the x-axis tick for a day.
export function formatDateTick(value: string, locale?: string): string {
  const d = parseDay(value)
  return d ? new Intl.DateTimeFormat(locale, {month: 'short', day: 'numeric'}).format(d) : value
}

// "Wed, Sep 24, 2026" — the tooltip header for a day.
export function formatDateLong(value: string, locale?: string): string {
  const d = parseDay(value)
  return d
    ? new Intl.DateTimeFormat(locale, {weekday: 'short', month: 'short', day: 'numeric', year: 'numeric'}).format(d)
    : value
}

export type DeltaDirection = 'up' | 'down' | 'flat' | 'none'

// formatDelta renders a change against a prior value: signed percent when the
// prior value is non-zero, signed absolute otherwise, nothing when there is no
// prior value. The direction lets the caller colour it by "is up good here".
export function formatDelta(
  current: number,
  prev: number | null | undefined,
  locale?: string,
): {text: string; direction: DeltaDirection} {
  if (prev === null || prev === undefined || !Number.isFinite(prev) || !Number.isFinite(current)) {
    return {text: '', direction: 'none'}
  }
  const diff = current - prev
  if (Math.abs(diff) < 1e-9) return {text: '±0', direction: 'flat'}
  const direction: DeltaDirection = diff > 0 ? 'up' : 'down'
  if (prev !== 0) {
    const pct = new Intl.NumberFormat(locale, {style: 'percent', maximumFractionDigits: 0, signDisplay: 'always'})
    return {text: pct.format(diff / Math.abs(prev)), direction}
  }
  const abs = new Intl.NumberFormat(locale, {maximumFractionDigits: 1, signDisplay: 'always'})
  return {text: abs.format(diff), direction}
}
