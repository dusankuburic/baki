import {request} from './client'

// Bulk flow uploads and folder loads can involve parsing many/large files
// server-side, well beyond the default request timeout.
const BULK_LOAD_TIMEOUT_MS = 300_000
import {createAdapter} from '@/platform/adapters'
import type {FlowDocument, RecentFile, SearchQuery, SearchResults, SourceFileInfo} from '@/types'

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

    // Check if it's a web upload (JSON content) or a Tauri path
    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return request('/api/flow/upload', data, 'POST', BULK_LOAD_TIMEOUT_MS)
        }
      } catch {
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
          return request('/api/flow/upload', data, 'POST', BULK_LOAD_TIMEOUT_MS)
        }
      } catch {
        // Not a JSON string, must be a path
      }
    }

    return request('/api/flow/load-folder', {path: result}, 'POST', BULK_LOAD_TIMEOUT_MS)
  },

  loadFlowFromPath: (path: string): Promise<FlowDocument | null> => request('/api/flow/load-path', {path}),

  loadFlowFolder: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-folder', {path}, 'POST', BULK_LOAD_TIMEOUT_MS),

  uploadFlow: (name: string, files: Record<string, string>): Promise<FlowDocument | null> =>
    request('/api/flow/upload', {name, files}, 'POST', BULK_LOAD_TIMEOUT_MS),

  recentFiles: (): Promise<RecentFile[]> => request('/api/flow/recent', undefined, 'GET'),

  removeRecentFile: (path: string): Promise<void> => request('/api/flow/remove-recent', {path}),

  clearRecentFiles: (): Promise<void> => request('/api/flow/clear-recent'),

  searchFlow: (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    request('/api/flow/search', {id: flowId, query}),

  revealInFileManager: (path: string): Promise<void> => request('/api/flow/reveal', {path}),

  getSourceFiles: (): Promise<SourceFileInfo[]> => request('/api/flow/source-files', undefined, 'GET'),

  // suppressInSource writes a `# pad-ignore[ruleId]` directive into the flow's
  // source file before the given block and returns the RE-PARSED document
  // (desktop only — cloud has no on-disk source). The suppression then travels
  // with the file (honored by the analyzer, CLI, baselines, CI), unlike a
  // UI-only suppression. Callers should setDocument(result) and re-analyze.
  suppressInSource: (flowId: string, blockId: string, ruleId: string): Promise<FlowDocument> =>
    request('/api/flow/suppress-in-source', {flowId, blockId, ruleId}),

  // applyFix applies a deterministic auto-fix (e.g. wrap-in-error-handler) to a
  // block in the flow's source file and returns the re-parsed document. Desktop
  // only. The finding carries the available fixType in its `autoFix` field;
  // show "Apply fix" only when that is set.
  applyFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
  ): Promise<FlowDocument> => request('/api/flow/apply-fix', {flowId, blockId, fixType, ruleId, variable, property}),

  previewFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
  ): Promise<{original: string; patched: string}> =>
    request('/api/flow/preview-fix', {flowId, blockId, fixType, ruleId, variable, property}),

  // reimport re-reads the currently-loaded flow's source file (desktop), re-
  // parses it, and returns the fresh document — so a user who edited the flow
  // in PAD can refresh in one click. Callers should setDocument(result) + re-
  // analyze to show what changed.
  reimport: (flowId: string): Promise<FlowDocument> => request('/api/flow/reimport', {flowId}),

  // Share links (read-only public report; cloud mode only)
  createShare: (flowId: string): Promise<{id: string; token: string; expiresAt?: string}> =>
    request('/api/flow/share/create', {flowId}),

  listShares: (flowId: string): Promise<unknown[]> => request('/api/flow/share/list', {flowId}),

  revokeShare: (flowId: string, tokenId: string): Promise<void> => request('/api/flow/share/revoke', {flowId, tokenId}),
}
