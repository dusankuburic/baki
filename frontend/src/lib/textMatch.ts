export function matchesQuery(haystack: string, query: string): boolean {
  const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
  if (tokens.length === 0) return true
  const text = haystack.toLowerCase()
  return tokens.every(token => text.includes(token))
}
