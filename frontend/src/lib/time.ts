// Shared time formatting. `relativeTime` renders a compact age ("now", "3m",
// "5h", "3d", "2w") for list/detail timestamps; pair it with `absoluteTime` in a
// `title=` so the exact moment is available on hover.

export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const secs = Math.max(0, (Date.now() - then) / 1000)
  if (secs < 60) return 'now'
  const mins = secs / 60
  if (mins < 60) return `${Math.floor(mins)}m`
  const hrs = mins / 60
  if (hrs < 24) return `${Math.floor(hrs)}h`
  const days = hrs / 24
  if (days < 7) return `${Math.floor(days)}d`
  return `${Math.floor(days / 7)}w`
}

export function absoluteTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleString()
}

const RELATIVE_UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 86400],
  ['month', 30 * 86400],
  ['week', 7 * 86400],
  ['day', 86400],
  ['hour', 3600],
  ['minute', 60],
]

// relativeTimeText renders a sentence-ready distance in either direction
// ("2 hours ago", "in 23 hours", "yesterday", "now") for prose such as
// "Signed in 2 hours ago", where the compact "2h" of relativeTime reads as a
// unit rather than a time. `now` and `locale` are parameters for tests.
export function relativeTimeText(iso: string, now = Date.now(), locale?: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const diff = (then - now) / 1000
  const rtf = new Intl.RelativeTimeFormat(locale, {numeric: 'auto'})
  for (const [unit, secs] of RELATIVE_UNITS) {
    if (Math.abs(diff) >= secs) return rtf.format(Math.round(diff / secs), unit)
  }
  return rtf.format(0, 'second')
}
