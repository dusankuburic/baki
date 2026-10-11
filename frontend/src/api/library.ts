import {request, requestValidated} from './client'
import {
  getLibraryFlowSchema,
  getLibraryFlowsPageSchema,
  getLibraryFlowVersionSchema,
  getLoadedFlowDocumentSchema,
} from './schemas'
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
  healthScore?: number
  errorCount?: number
  warningCount?: number
  tags?: string[]
}

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

export type PortfolioSort = 'risk' | 'health' | 'errors' | 'name' | 'analyzed'

export interface PortfolioQuery {
  orgId?: string
  sort?: PortfolioSort
  limit?: number
}

export interface Portfolio {
  entries: PortfolioEntry[]
  totalFlows: number
  rankedFlows: number
  truncated: boolean
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
  list: async (filter: LibraryFilter = {}, signal?: AbortSignal): Promise<PagedResponse<LibraryFlow>> => {
    const params = new URLSearchParams()
    if (filter.orgId) params.set('orgId', filter.orgId)
    if (filter.scope) params.set('scope', filter.scope)
    if (filter.sort) params.set('sort', filter.sort)
    if (filter.query) params.set('q', filter.query)
    if (filter.tags && filter.tags.length > 0) params.set('tags', filter.tags.join(','))
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return requestValidated(`/api/library${qs ? '?' + qs : ''}`, await getLibraryFlowsPageSchema(), {
      method: 'GET',
      signal,
    })
  },

  portfolio: (q: PortfolioQuery = {}, signal?: AbortSignal): Promise<Portfolio> => {
    const params = new URLSearchParams()
    if (q.orgId) params.set('orgId', q.orgId)
    if (q.sort) params.set('sort', q.sort)
    if (q.limit) params.set('limit', String(q.limit))
    const qs = params.toString()
    return request(`/api/library/portfolio${qs ? '?' + qs : ''}`, {method: 'GET', signal})
  },

  get: async (id: string, signal?: AbortSignal): Promise<LibraryFlow> =>
    requestValidated(`/api/library/${id}`, await getLibraryFlowSchema(), {method: 'GET', noDedup: true, signal}),

  getContent: async (id: string, signal?: AbortSignal): Promise<FlowDocument> =>
    requestValidated(`/api/library/${id}/content`, await getLoadedFlowDocumentSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    }),

  versions: async (id: string, limit?: number): Promise<LibraryFlowVersion[]> => {
    const qs = limit ? `?limit=${limit}` : ''
    return requestValidated(`/api/library/${id}/versions${qs}`, await getLibraryFlowVersionSchema(), {method: 'GET'})
  },

  create: (req: CreateLibraryFlowRequest): Promise<LibraryFlow> => request('/api/library', {body: req}),

  update: (id: string, req: UpdateLibraryFlowRequest): Promise<LibraryFlow> =>
    request(`/api/library/${id}`, {body: req, method: 'PUT'}),

  delete: (id: string): Promise<void> => request(`/api/library/${id}`, {method: 'DELETE'}),
}
