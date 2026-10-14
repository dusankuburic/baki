import {displayTimeZone} from '@/lib/time'

export function formatCompact(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

export function formatContextWindow(n: number): string {
  if (n >= 1_000_000) {
    const m = n / 1_000_000
    return `${Number.isInteger(m) ? m : m.toFixed(1)}M`
  }
  return `${Math.round(n / 1000)}k`
}

export function formatCount(n: number): string {
  return n.toLocaleString()
}

export function formatCompactIntl(n: number, locale?: string): string {
  return new Intl.NumberFormat(locale, {notation: 'compact', maximumFractionDigits: 1}).format(n)
}

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

export function parseDay(value: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  const d = m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : new Date(value)
  return Number.isNaN(d.getTime()) ? null : d
}

export function formatDateTick(value: string, locale?: string): string {
  const d = parseDay(value)
  return d ? new Intl.DateTimeFormat(locale, {month: 'short', day: 'numeric'}).format(d) : value
}

export function formatDateLong(value: string, locale?: string): string {
  const d = parseDay(value)
  return d
    ? new Intl.DateTimeFormat(locale, {weekday: 'short', month: 'short', day: 'numeric', year: 'numeric'}).format(d)
    : value
}

export function formatDateTime(value: string, locale?: string): string {
  const d = new Date(value)
  return Number.isNaN(d.getTime())
    ? value
    : new Intl.DateTimeFormat(locale, {dateStyle: 'medium', timeStyle: 'short', timeZone: displayTimeZone()}).format(d)
}

export function formatTimeOfDay(value: number | string, locale?: string): string {
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? String(value) : new Intl.DateTimeFormat(locale, {timeStyle: 'short', timeZone: displayTimeZone()}).format(d)
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

export type DeltaDirection = 'up' | 'down' | 'flat' | 'none'

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
