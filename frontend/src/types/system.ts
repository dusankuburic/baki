// Top-level system/runtime info (GET /api/system/info).

export interface AppInfo {
  version: string;
  platform: string;
  arch: string;
  buildDate: string;
  gitCommit: string;
}

// Generic envelope returned by paginated list endpoints (render.PagedResponse
// on the Go side). `items` holds the page; `total` is the unpaginated count
// so the UI can render "X of Y" and pager controls.
export interface PagedResponse<T> {
  items: T[];
  total: number;
  offset: number;
  limit: number;
}
