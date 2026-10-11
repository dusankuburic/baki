import {request, requestValidated} from './client'
import {
  getLoadedFlowDocumentSchema,
  getRecentFilesSchema,
  getSearchResultsSchema,
  getSnapshotsListSchema,
  getSourceFilesSchema,
  getSourceMetaSchema,
} from './schemas'
import type {FlowDocument, RecentFile, SearchQuery, SearchResults, SourceFileInfo} from '@/types'

const BULK_LOAD_TIMEOUT_MS = 300_000
import {createAdapter} from '@/platform/adapters'

export interface ShareInfo {
  id: string
  token?: string
  createdAt?: string
  expiresAt?: string
}

export const flowApi = {
  openFlowFile: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const result = await adapter.fileOpen({
      filters: [
        {name: 'PAD Flow Export', extensions: ['txt']},
        {name: 'All Files', extensions: ['*']},
      ],
    })
    if (!result) return null

    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return requestValidated('/api/flow/upload', await getLoadedFlowDocumentSchema(), {
            body: data,
            method: 'POST',
            timeoutMs: BULK_LOAD_TIMEOUT_MS,
          })
        }
      } catch {
      }
    }

    const finalPath = Array.isArray(result) ? result[0] : result
    return requestValidated('/api/flow/load-path', await getLoadedFlowDocumentSchema(), {body: {path: finalPath}})
  },

  openFlowFolder: async (): Promise<FlowDocument | null> => {
    const adapter = createAdapter()
    const result = await adapter.fileOpenDirectory()
    if (!result) return null

    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return requestValidated('/api/flow/upload', await getLoadedFlowDocumentSchema(), {
            body: data,
            method: 'POST',
            timeoutMs: BULK_LOAD_TIMEOUT_MS,
          })
        }
      } catch {
      }
    }

    return requestValidated('/api/flow/load-folder', await getLoadedFlowDocumentSchema(), {
      body: {path: result},
      method: 'POST',
      timeoutMs: BULK_LOAD_TIMEOUT_MS,
    })
  },

  loadFlowFromPath: async (path: string): Promise<FlowDocument | null> =>
    requestValidated('/api/flow/load-path', await getLoadedFlowDocumentSchema(), {body: {path}}),

  loadFlowFolder: async (path: string): Promise<FlowDocument | null> =>
    requestValidated('/api/flow/load-folder', await getLoadedFlowDocumentSchema(), {
      body: {path},
      method: 'POST',
      timeoutMs: BULK_LOAD_TIMEOUT_MS,
    }),

  uploadFlow: (name: string, files: Record<string, string>): Promise<FlowDocument | null> =>
    request('/api/flow/upload', {body: {name, files}, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS}),

  recentFiles: async (): Promise<RecentFile[]> =>
    requestValidated('/api/flow/recent', await getRecentFilesSchema(), {method: 'GET'}),

  removeRecentFile: (path: string): Promise<void> => request('/api/flow/remove-recent', {body: {path}}),

  clearRecentFiles: (): Promise<void> => request('/api/flow/clear-recent'),

  searchFlow: async (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    requestValidated('/api/flow/search', await getSearchResultsSchema(), {body: {id: flowId, query}}),

  searchLibrary: async (query: SearchQuery): Promise<SearchResults> =>
    requestValidated('/api/flow/search-library', await getSearchResultsSchema(), {body: {query}}),

  revealInFileManager: (path: string): Promise<void> => request('/api/flow/reveal', {body: {path}}),

  getSourceFiles: async (): Promise<SourceFileInfo[]> =>
    requestValidated('/api/flow/source-files', await getSourceFilesSchema(), {method: 'GET'}),

  suppressInSource: (flowId: string, blockId: string, ruleId: string): Promise<FlowDocument> =>
    request('/api/flow/suppress-in-source', {body: {flowId, blockId, ruleId}}),

  applyFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
    unsafe?: boolean,
  ): Promise<FlowDocument> =>
    request('/api/flow/apply-fix', {body: {flowId, blockId, fixType, ruleId, variable, property, unsafe}}),

  previewFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
  ): Promise<{original: string; patched: string}> =>
    request('/api/flow/preview-fix', {body: {flowId, blockId, fixType, ruleId, variable, property}}),

  applyFixBatch: (
    flowId: string,
    rules: string[],
    limit?: number,
  ): Promise<{document: FlowDocument; applied: number; partial?: boolean; partialReason?: string}> =>
    request('/api/flow/apply-fix-batch', {body: {flowId, rules, limit}}),

  listSnapshots: async (
    flowId: string,
  ): Promise<{snapshots: {id: string; label: string; createdAt: string; bytes: number}[]}> =>
    requestValidated(`/api/flow/snapshots${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, await getSnapshotsListSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  restoreSnapshot: (flowId: string, snapshotId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/snapshots/restore', {body: {flowId, snapshotId}}),

  updateBlockProperties: (
    flowId: string,
    blockId: string,
    changes: Record<string, string>,
  ): Promise<{document: FlowDocument}> => request('/api/flow/block/properties', {body: {flowId, blockId, changes}}),

  moveBlock: (flowId: string, blockId: string, direction: 'up' | 'down'): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/move', {body: {flowId, blockId, direction}}),

  moveBlockTo: (
    flowId: string,
    blockId: string,
    refBlockId: string,
    position: 'before' | 'after',
  ): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/move-to', {body: {flowId, blockId, refBlockId, position}}),

  removeBlock: (flowId: string, blockId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/remove', {body: {flowId, blockId}}),

  removeBlocks: (flowId: string, blockIds: string[]): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/remove-batch', {body: {flowId, blockIds}}),

  renameBlock: (
    flowId: string,
    blockId: string,
    name: string,
  ): Promise<{document: FlowDocument; gotoRefsUpdated: number}> =>
    request('/api/flow/block/rename', {body: {flowId, blockId, name}}),

  duplicateBlock: (flowId: string, blockId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/duplicate', {body: {flowId, blockId}}),

  setTags: (flowId: string, tags: string[]): Promise<{tags: string[]}> =>
    request('/api/flow/tags', {body: {flowId, tags}, method: 'PUT'}),

  getSourceMeta: async (flowId: string): Promise<{size: number; modTime: string; files: number}> =>
    requestValidated(`/api/flow/source-meta${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, await getSourceMetaSchema(), {
      method: 'GET',
    }),

  getSource: (flowId: string, file?: string): Promise<{source: string}> => {
    const params = new URLSearchParams()
    if (flowId) params.set('flowId', flowId)
    if (file) params.set('file', file)
    const qs = params.toString()
    return request(`/api/flow/source${qs ? '?' + qs : ''}`, {method: 'GET'})
  },

  saveSource: (flowId: string, source: string, file?: string): Promise<FlowDocument> =>
    request('/api/flow/save-source', {body: {flowId, source, file}}),

  reimport: (flowId: string): Promise<FlowDocument> => request('/api/flow/reimport', {body: {flowId}}),

  createShare: (flowId: string): Promise<{id: string; token: string; expiresAt?: string}> =>
    request('/api/flow/share/create', {body: {flowId}}),

  listShares: (flowId: string): Promise<ShareInfo[]> => request('/api/flow/share/list', {body: {flowId}}),

  revokeShare: (flowId: string, tokenId: string): Promise<void> =>
    request('/api/flow/share/revoke', {body: {flowId, tokenId}}),
}
