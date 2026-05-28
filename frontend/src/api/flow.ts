import {request} from './client'
import { createAdapter } from '@/platform/adapters'
import type {FlowDocument, RecentFile, SearchQuery, SearchResults} from '@/types/domain'

export const flowApi = {
  openFlowFile: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const path = await adapter.fileOpen({
      filters: [{name: 'PAD Flow Export', extensions: ['txt']}, {name: 'All Files', extensions: ['*']}]
    })
    if (!path) return null
    const finalPath = Array.isArray(path) ? path[0] : path
    return request('/api/flow/load-path', {path: finalPath})
  },

  openFlowFolder: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const path = await adapter.fileOpenDirectory()
    if (!path) return null
    return request('/api/flow/load-folder', {path})
  },

  loadFlowFromPath: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-path', {path}),

  loadFlowFolder: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-folder', {path}),

  recentFiles: (): Promise<RecentFile[]> =>
    request('/api/flow/recent', undefined, 'GET'),

  removeRecentFile: (path: string): Promise<void> =>
    request('/api/flow/remove-recent', {path}),

  clearRecentFiles: (): Promise<void> =>
    request('/api/flow/clear-recent'),

  searchFlow: (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    request('/api/flow/search', {flowId, query}),

  revealInFileManager: (path: string): Promise<void> =>
    request('/api/flow/reveal', {path}),

  getSourceFiles: (): Promise<any[]> =>
    request('/api/flow/source-files', undefined, 'GET'),
}
