import {request} from './client'
import { createAdapter } from '@/platform/adapters'
import type {FlowDocument, RecentFile, SearchQuery, SearchResults} from '@/types/domain'

export const flowApi = {
  openFlowFile: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const result = await adapter.fileOpen({
      filters: [{name: 'PAD Flow Export', extensions: ['txt']}, {name: 'All Files', extensions: ['*']}]
    })
    if (!result) return null
    
    // Check if it's a web upload (JSON content) or a Tauri path
    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return request('/api/flow/upload', data)
        }
      } catch (e) {
        // Not a JSON string, must be a path
      }
    }

    const finalPath = Array.isArray(result) ? result[0] : result
    return request('/api/flow/load-path', {path: finalPath})
  },

  openFlowFolder: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const result = await adapter.fileOpenDirectory()
    if (!result) return null

    // Check if it's a web upload (JSON content) or a Tauri path
    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return request('/api/flow/upload', data)
        }
      } catch (e) {
        // Not a JSON string, must be a path
      }
    }

    return request('/api/flow/load-folder', {path: result})
  },

  loadFlowFromPath: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-path', {path}),

  loadFlowFolder: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-folder', {path}),

  uploadFlow: (name: string, files: Record<string, string>): Promise<FlowDocument | null> =>
    request('/api/flow/upload', {name, files}),

  recentFiles: (): Promise<RecentFile[]> =>
    request('/api/flow/recent', undefined, 'GET'),

  removeRecentFile: (path: string): Promise<void> =>
    request('/api/flow/remove-recent', {path}),

  clearRecentFiles: (): Promise<void> =>
    request('/api/flow/clear-recent'),

  searchFlow: (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    request('/api/flow/search', {id: flowId, query}),

  revealInFileManager: (path: string): Promise<void> =>
    request('/api/flow/reveal', {path}),

  getSourceFiles: (): Promise<any[]> =>
    request('/api/flow/source-files', undefined, 'GET'),
}
