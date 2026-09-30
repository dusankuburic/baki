// Search: every whitespace-separated token of the query has to occur in the
// haystack, in any order and case. Shared by the Settings search (which
// matches a section's label, description, group label and keywords) and any
// other client-side filter that should feel the same.
export function matchesQuery(haystack: string, query: string): boolean {
  const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
  if (tokens.length === 0) return true
  const text = haystack.toLowerCase()
  return tokens.every(token => text.includes(token))
}
