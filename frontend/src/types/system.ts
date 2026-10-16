
export interface AppInfo {
  version: string
  platform: string
  arch: string
  buildDate: string
  gitCommit: string
  capabilities: AppCapabilities
}

export interface AppCapabilities {
  sessionAnalytics: boolean
}

export interface PagedResponse<T> {
  items: T[]
  total: number
  offset: number
  limit: number
  // Since the keyset cursors (absent from an older server): whether items follow the page, the ?cursor= of the next
  // page (lists that page by keyset, omitted on the last page), and whether total stopped at the list's cap.
  hasMore?: boolean
  nextCursor?: string
  totalCapped?: boolean
}
