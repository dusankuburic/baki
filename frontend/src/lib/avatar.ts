export function userInitials(name: string): string {
  const parts = name.trim().split(/\s+/)
  const first = parts[0] ?? ''
  if (parts.length === 1) return first.slice(0, 2).toUpperCase()
  const last = parts[parts.length - 1] ?? ''
  return (first.charAt(0) + last.charAt(0)).toUpperCase()
}

const PALETTE = ['#5b61ef', '#8b5cf6', '#ec4899', '#ef4444', '#f97316', '#eab308', '#22c55e', '#06b6d4']

export function userColor(userId: string): string {
  let hash = 0
  for (let i = 0; i < userId.length; i++) {
    hash = (hash * 31 + userId.charCodeAt(i)) >>> 0
  }
  // The modulo keeps the index inside PALETTE; the fallback (= PALETTE[0])
  // only satisfies the index-access type.
  return PALETTE[hash % PALETTE.length] ?? '#5b61ef'
}
