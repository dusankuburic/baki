import {request} from './client'
import type {FlowDocument, RecentFile, SearchQuery, SearchResults, SourceFileInfo} from '@/types'

// Bulk flow uploads and folder loads can involve parsing many/large files
// server-side, well beyond the default request timeout.
const BULK_LOAD_TIMEOUT_MS = 300_000
import {createAdapter} from '@/platform/adapters'

// ShareInfo is one row of a flow's read-only share-link list. Mirrors the
// backend share-token record (createShare returns the same fields plus the raw
// secret, which is never re-sent by the list endpoint).
export interface ShareInfo {
  id: string
  token: string
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

    // Check if it's a web upload (JSON content) or a Tauri path
    if (typeof result === 'string') {
      try {
        const data = JSON.parse(result)
        if (data && data.__is_web_upload__) {
          return request('/api/flow/upload', {body: data, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS})
        }
      } catch {
        // Not a JSON string, must be a path
      }
    }

    const finalPath = Array.isArray(result) ? result[0] : result
    return request('/api/flow/load-path', {body: {path: finalPath}})
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
          return request('/api/flow/upload', {body: data, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS})
        }
      } catch {
        // Not a JSON string, must be a path
      }
    }

    return request('/api/flow/load-folder', {body: {path: result}, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS})
  },

  loadFlowFromPath: (path: string): Promise<FlowDocument | null> => request('/api/flow/load-path', {body: {path}}),

  loadFlowFolder: (path: string): Promise<FlowDocument | null> =>
    request('/api/flow/load-folder', {body: {path}, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS}),

  uploadFlow: (name: string, files: Record<string, string>): Promise<FlowDocument | null> =>
    request('/api/flow/upload', {body: {name, files}, method: 'POST', timeoutMs: BULK_LOAD_TIMEOUT_MS}),

  recentFiles: (): Promise<RecentFile[]> => request('/api/flow/recent', {method: 'GET'}),

  removeRecentFile: (path: string): Promise<void> => request('/api/flow/remove-recent', {body: {path}}),

  clearRecentFiles: (): Promise<void> => request('/api/flow/clear-recent'),

  searchFlow: (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    request('/api/flow/search', {body: {id: flowId, query}}),

  // searchLibrary runs the query across every flow the caller can access
  // (cross-flow / org-wide search); each hit carries flowId/flowName so the UI
  // can group results by flow.
  searchLibrary: (query: SearchQuery): Promise<SearchResults> => request('/api/flow/search-library', {body: {query}}),

  revealInFileManager: (path: string): Promise<void> => request('/api/flow/reveal', {body: {path}}),

  getSourceFiles: (): Promise<SourceFileInfo[]> => request('/api/flow/source-files', {method: 'GET'}),

  // suppressInSource writes a `# pad-ignore[ruleId]` directive into the flow's
  // source file before the given block and returns the RE-PARSED document
  // (desktop only — cloud has no on-disk source). The suppression then travels
  // with the file (honored by the analyzer, CLI, baselines, CI), unlike a
  // UI-only suppression. Callers should setDocument(result) and re-analyze.
  suppressInSource: (flowId: string, blockId: string, ruleId: string): Promise<FlowDocument> =>
    request('/api/flow/suppress-in-source', {body: {flowId, blockId, ruleId}}),

  // applyFix applies a deterministic auto-fix (e.g. wrap-error-handler) to a
  // block in the flow's source and returns the re-parsed document. Works on
  // desktop (file-backed) and cloud flows — single-file (stored source) AND
  // folder flows (per-member-file patching via the canonical serializer, R3-3).
  // The finding carries the available fixType in its `autoFix` field; show
  // "Apply fix" only when that is set.
  applyFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
  ): Promise<FlowDocument> =>
    request('/api/flow/apply-fix', {body: {flowId, blockId, fixType, ruleId, variable, property}}),

  previewFix: (
    flowId: string,
    blockId: string,
    fixType: string,
    ruleId?: string,
    variable?: string,
    property?: string,
  ): Promise<{original: string; patched: string}> =>
    request('/api/flow/preview-fix', {body: {flowId, blockId, fixType, ruleId, variable, property}}),

  // applyFixBatch applies every auto-fixable finding whose rule is in `rules`
  // (empty = all auto-fixable) in one server-side pass, returning the updated
  // document + how many fixes landed. Used by the bulk-action bar. Works in
  // desktop and cloud (single-file) modes.
  applyFixBatch: (
    flowId: string,
    rules: string[],
    limit?: number,
  ): Promise<{document: FlowDocument; applied: number}> =>
    request('/api/flow/apply-fix-batch', {body: {flowId, rules, limit}}),

  // Snapshot (undo) API: every fix/batch/source-save captures the pre-mutation
  // source server-side; restore writes it back (desktop: file; cloud: OCC
  // persist) and returns the re-loaded document.
  listSnapshots: (flowId: string): Promise<{snapshots: {id: string; label: string; createdAt: string; bytes: number}[]}> =>
    request(`/api/flow/snapshots${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, {method: 'GET'}),

  restoreSnapshot: (flowId: string, snapshotId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/snapshots/restore', {body: {flowId, snapshotId}}),

  // Property editing + reordering (R3-2): targeted in-line property replaces
  // (other properties' text untouched) and sibling moves. Same undo/parse-gate
  // guarantees as the other block edits.
  updateBlockProperties: (flowId: string, blockId: string, changes: Record<string, string>): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/properties', {body: {flowId, blockId, changes}}),

  moveBlock: (flowId: string, blockId: string, direction: 'up' | 'down'): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/move', {body: {flowId, blockId, direction}}),

  // moveBlockTo reorders before/after a reference sibling — the primitive
  // drag-and-drop maps to. Same-scope only (cross-container refused server-side).
  moveBlockTo: (
    flowId: string,
    blockId: string,
    refBlockId: string,
    position: 'before' | 'after',
  ): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/move-to', {body: {flowId, blockId, refBlockId, position}}),

  // Block editing (R3-1b): remove deletes the block (+ descendants) from the
  // source; duplicate inserts a verbatim copy after it. Both parse-gated and
  // snapshotted server-side (undo via listSnapshots/restoreSnapshot).
  removeBlock: (flowId: string, blockId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/remove', {body: {flowId, blockId}}),

  // removeBlocks bulk-deletes in ONE server-side patch (U3b multi-select).
  removeBlocks: (flowId: string, blockIds: string[]): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/remove-batch', {body: {flowId, blockIds}}),

  // renameBlock renames LABEL/COMMENT blocks; labels rewrite their GOTO refs.
  renameBlock: (flowId: string, blockId: string, name: string): Promise<{document: FlowDocument; gotoRefsUpdated: number}> =>
    request('/api/flow/block/rename', {body: {flowId, blockId, name}}),

  duplicateBlock: (flowId: string, blockId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/duplicate', {body: {flowId, blockId}}),

  // setTags replaces a cloud flow's organizational tags (R2-4b). Server
  // normalizes (lowercase, letters/digits/-/_, ≤32 chars, ≤20 tags) and
  // returns the canonical set.
  setTags: (flowId: string, tags: string[]): Promise<{tags: string[]}> =>
    request('/api/flow/tags', {body: {flowId, tags}, method: 'PUT'}),

  // getSourceMeta returns the cheap change-detection signal (size + mtime;
  // folder flows aggregate members) the desktop watcher polls.
  getSourceMeta: (flowId: string): Promise<{size: number; modTime: string; files: number}> =>
    request(`/api/flow/source-meta${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, {method: 'GET'}),

  // getSource returns the raw PAD source text (desktop: file; cloud: stored).
  getSource: (flowId: string): Promise<{source: string}> =>
    request(`/api/flow/source${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, {method: 'GET'}),

  // saveSource replaces the raw source, re-parses, and returns the updated doc.
  saveSource: (flowId: string, source: string): Promise<FlowDocument> =>
    request('/api/flow/save-source', {body: {flowId, source}}),

  // reimport re-reads the currently-loaded flow's source file (desktop), re-
  // parses it, and returns the fresh document — so a user who edited the flow
  // in PAD can refresh in one click. Callers should setDocument(result) + re-
  // analyze to show what changed.
  reimport: (flowId: string): Promise<FlowDocument> => request('/api/flow/reimport', {body: {flowId}}),

  // Share links (read-only public report; cloud mode only)
  createShare: (flowId: string): Promise<{id: string; token: string; expiresAt?: string}> =>
    request('/api/flow/share/create', {body: {flowId}}),

  listShares: (flowId: string): Promise<ShareInfo[]> => request('/api/flow/share/list', {body: {flowId}}),

  revokeShare: (flowId: string, tokenId: string): Promise<void> =>
    request('/api/flow/share/revoke', {body: {flowId, tokenId}}),
}
