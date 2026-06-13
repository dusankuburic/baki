import { request } from './client'
import type { FlowDocument } from '@/types/domain'

export interface LibraryFlow {
  id: string
  name: string
  description?: string
  ownerId: string
  ownerDisplayName?: string
  blockCount: number
  subflowCount: number
  updatedAt: string
  version: number
  isSharedWithMe: boolean
  canEdit: boolean
  canDelete: boolean
  canShare: boolean
}

export interface LibraryFilter {
  orgId?: string
  query?: string
  limit?: number
  offset?: number
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
  list: (filter: LibraryFilter = {}): Promise<LibraryFlow[]> => {
    const params = new URLSearchParams()
    if (filter.orgId) params.set('orgId', filter.orgId)
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

  create: (req: CreateLibraryFlowRequest): Promise<LibraryFlow> =>
    request('/api/library', req),

  update: (id: string, req: UpdateLibraryFlowRequest): Promise<LibraryFlow> =>
    request(`/api/library/${id}`, req, 'PUT'),

  delete: (id: string): Promise<void> =>
    request(`/api/library/${id}`, undefined, 'DELETE'),
}
