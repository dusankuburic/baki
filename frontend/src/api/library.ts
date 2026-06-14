import { request } from './client'
import type { FlowDocument, PagedResponse } from '@/types'

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
}

export type LibraryScope = 'all' | 'mine' | 'shared'
export type LibrarySort = 'updated_desc' | 'updated_asc' | 'name_asc' | 'name_desc' | 'blocks_desc'

export interface LibraryFilter {
  orgId?: string
  scope?: LibraryScope
  sort?: LibrarySort
  query?: string
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
  list: (filter: LibraryFilter = {}): Promise<PagedResponse<LibraryFlow>> => {
    const params = new URLSearchParams()
    if (filter.orgId) params.set('orgId', filter.orgId)
    if (filter.scope) params.set('scope', filter.scope)
    if (filter.sort) params.set('sort', filter.sort)
    if (filter.query) params.set('q', filter.query)
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return request(`/api/library${qs ? '?' + qs : ''}`, undefined, 'GET')
  },

  get: (id: string): Promise<LibraryFlow> =>
    request(`/api/library/${id}`, undefined, 'GET'),

  getContent: (id: string): Promise<FlowDocument> =>
    request(`/api/library/${id}/content`, undefined, 'GET'),

  versions: (id: string, limit?: number): Promise<LibraryFlowVersion[]> => {
    const qs = limit ? `?limit=${limit}` : ''
    return request(`/api/library/${id}/versions${qs}`, undefined, 'GET')
  },

  create: (req: CreateLibraryFlowRequest): Promise<LibraryFlow> =>
    request('/api/library', req),

  update: (id: string, req: UpdateLibraryFlowRequest): Promise<LibraryFlow> =>
    request(`/api/library/${id}`, req, 'PUT'),

  delete: (id: string): Promise<void> =>
    request(`/api/library/${id}`, undefined, 'DELETE'),
}
