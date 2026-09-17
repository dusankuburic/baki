import {request} from './client'
import type {FlowDocument, PagedResponse} from '@/types'

export interface LibraryFlow {
  id: string
  name: string
  description?: string
  ownerId: string
  ownerDisplayName?: string
  organizationId?: string
  blockCount: number
  subflowCount: number
  updatedAt: string
  version: number
  isSharedWithMe: boolean
  canEdit: boolean
  canDelete: boolean
  canShare: boolean
  // Present only on single-flow GET (not the list endpoint).
  healthScore?: number
  errorCount?: number
  warningCount?: number
  /** Org tags (backend-normalized). */
  tags?: string[]
}

// Portfolio: the org-wide governance fleet view (GET /api/library/portfolio).
export interface PortfolioEntry {
  flowId: string
  flowName: string
  ownerId?: string
  ownerName?: string
  analyzed: boolean
  healthScore: number
  errors: number
  warnings: number
  info: number
  analyzedAt?: string
}

/** Portfolio entry order; the rollup totals always cover every ranked flow. */
export type PortfolioSort = 'risk' | 'health' | 'errors' | 'name' | 'analyzed'

export interface PortfolioQuery {
  orgId?: string
  sort?: PortfolioSort
  /** Maximum entries returned; omitted means every ranked flow (server cap 1000). */
  limit?: number
}

export interface Portfolio {
  entries: PortfolioEntry[]
  totalFlows: number
  analyzedFlows: number
  avgHealth: number
  errors: number
  warnings: number
  info: number
}

export type LibraryScope = 'all' | 'mine' | 'shared'
export type LibrarySort = 'updated_desc' | 'updated_asc' | 'name_asc' | 'name_desc' | 'blocks_desc'

export interface LibraryFilter {
  orgId?: string
  scope?: LibraryScope
  sort?: LibrarySort
  query?: string
  /** ANY-of tag match (backend-normalized tags). */
  tags?: string[]
  limit?: number
  offset?: number
}

export interface LibraryFlowVersion {
  id: string
  flowId: string
  version: number
  comment: string
  createdBy: string
  createdAt: string
}

export interface CreateLibraryFlowRequest {
  name: string
  description?: string
  orgId?: string
  content: FlowDocument
}

export interface UpdateLibraryFlowRequest {
  name?: string
  description?: string
  content?: unknown
  version: number
}

export const libraryApi = {
  list: (filter: LibraryFilter = {}, signal?: AbortSignal): Promise<PagedResponse<LibraryFlow>> => {
    const params = new URLSearchParams()
    if (filter.orgId) params.set('orgId', filter.orgId)
    if (filter.scope) params.set('scope', filter.scope)
    if (filter.sort) params.set('sort', filter.sort)
    if (filter.query) params.set('q', filter.query)
    if (filter.tags && filter.tags.length > 0) params.set('tags', filter.tags.join(','))
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return request(`/api/library${qs ? '?' + qs : ''}`, {method: 'GET', signal})
  },

  // Org-wide governance portfolio, ranked worst-health-first unless sort says otherwise.
  portfolio: (q: PortfolioQuery = {}, signal?: AbortSignal): Promise<Portfolio> => {
    const params = new URLSearchParams()
    if (q.orgId) params.set('orgId', q.orgId)
    if (q.sort) params.set('sort', q.sort)
    if (q.limit) params.set('limit', String(q.limit))
    const qs = params.toString()
    return request(`/api/library/portfolio${qs ? '?' + qs : ''}`, {method: 'GET', signal})
  },

  get: (id: string): Promise<LibraryFlow> => request(`/api/library/${id}`, {method: 'GET'}),

  getContent: (id: string): Promise<FlowDocument> => request(`/api/library/${id}/content`, {method: 'GET'}),

  versions: (id: string, limit?: number): Promise<LibraryFlowVersion[]> => {
    const qs = limit ? `?limit=${limit}` : ''
    return request(`/api/library/${id}/versions${qs}`, {method: 'GET'})
  },

  create: (req: CreateLibraryFlowRequest): Promise<LibraryFlow> => request('/api/library', {body: req}),

  update: (id: string, req: UpdateLibraryFlowRequest): Promise<LibraryFlow> =>
    request(`/api/library/${id}`, {body: req, method: 'PUT'}),

  delete: (id: string): Promise<void> => request(`/api/library/${id}`, {method: 'DELETE'}),
}
