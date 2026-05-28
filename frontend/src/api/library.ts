import { request } from './client'
import type { FlowDocument } from '@/types/domain'

export interface LibraryFlow {
  id: string
  name: string
  description?: string
  ownerDisplayName?: string
  blockCount: number
  subflowCount: number
  updatedAt: string
  isSharedWithMe: boolean
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
  flowDocument: FlowDocument
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

  delete: (id: string): Promise<void> =>
    request(`/api/library/${id}`, undefined, 'DELETE'),
}
