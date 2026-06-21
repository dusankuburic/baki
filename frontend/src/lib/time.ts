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
