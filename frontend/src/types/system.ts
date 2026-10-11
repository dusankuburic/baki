
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
}
