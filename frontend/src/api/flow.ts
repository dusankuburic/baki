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

// Bulk flow uploads and folder loads can involve parsing many/large files
// server-side, well beyond the default request timeout.
const BULK_LOAD_TIMEOUT_MS = 300_000
import {createAdapter} from '@/platform/adapters'

// ShareInfo is one row of a flow's read-only share-link list. Mirrors the
// backend share-token record. The raw secret is returned ONLY by createShare
// (once); the list endpoint carries the hash, so `token` is never populated
// here — the UI must capture it at creation time.
export interface ShareInfo {
  id: string
  token?: string
  createdAt?: string
  expiresAt?: string
}

export const flowApi = {
  // load-path / load-folder are reads that answer the parsed document; the
  // envelope check (id, name, subflows) is the same one the flow:loaded
  // re-fetch uses. The mutation POSTs (upload / apply-fix / save-source) are
  // deliberately NOT validated — see the report on each below.
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
          return requestValidated('/api/flow/upload', await getLoadedFlowDocumentSchema(), {
            body: data,
            method: 'POST',
            timeoutMs: BULK_LOAD_TIMEOUT_MS,
          })
        }
      } catch {
        // Not a JSON string, must be a path
      }
    }

    const finalPath = Array.isArray(result) ? result[0] : result
    return requestValidated('/api/flow/load-path', await getLoadedFlowDocumentSchema(), {body: {path: finalPath}})
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
          return requestValidated('/api/flow/upload', await getLoadedFlowDocumentSchema(), {
            body: data,
            method: 'POST',
            timeoutMs: BULK_LOAD_TIMEOUT_MS,
          })
        }
      } catch {
        // Not a JSON string, must be a path
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

  // The recent list is `null` on the wire when empty (Go nil slice); the
  // schema reads that as [].
  recentFiles: async (): Promise<RecentFile[]> =>
    requestValidated('/api/flow/recent', await getRecentFilesSchema(), {method: 'GET'}),

  removeRecentFile: (path: string): Promise<void> => request('/api/flow/remove-recent', {body: {path}}),

  clearRecentFiles: (): Promise<void> => request('/api/flow/clear-recent'),

  searchFlow: async (flowId: string, query: SearchQuery): Promise<SearchResults> =>
    requestValidated('/api/flow/search', await getSearchResultsSchema(), {body: {id: flowId, query}}),

  // searchLibrary runs the query across every flow the caller can access
  // (cross-flow / org-wide search); each hit carries flowId/flowName so the UI
  // can group results by flow.
  searchLibrary: async (query: SearchQuery): Promise<SearchResults> =>
    requestValidated('/api/flow/search-library', await getSearchResultsSchema(), {body: {query}}),

  revealInFileManager: (path: string): Promise<void> => request('/api/flow/reveal', {body: {path}}),

  getSourceFiles: async (): Promise<SourceFileInfo[]> =>
    requestValidated('/api/flow/source-files', await getSourceFilesSchema(), {method: 'GET'}),

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
  // `unsafe` is the explicit opt-in for behaviour-changing fixers
  // (replace-with-variable & co.): the server answers 400 without it. Pass
  // true only after the preview diff has been shown to the user.
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

  // applyFixBatch applies every auto-fixable finding whose rule is in `rules`
  // (empty = all auto-fixable) in one server-side pass, returning the updated
  // document + how many fixes landed. Used by the bulk-action bar. Works in
  // desktop and cloud (single-file) modes. `partial` means the request's time
  // budget ended the loop early: the fixes applied so far ARE persisted (the
  // batch used to discard all of them), and running again continues.
  applyFixBatch: (
    flowId: string,
    rules: string[],
    limit?: number,
  ): Promise<{document: FlowDocument; applied: number; partial?: boolean; partialReason?: string}> =>
    request('/api/flow/apply-fix-batch', {body: {flowId, rules, limit}}),

  // Snapshot (undo) API: every fix/batch/source-save captures the pre-mutation
  // source server-side; restore writes it back (desktop: file; cloud: OCC
  // persist) and returns the re-loaded document.
  // noDedup: the newest entry is captured right before each mutation as its
  // undo target, and consecutive edits (held move key) land within the 5 s
  // micro-cache window — a cached list named the wrong snapshot.
  listSnapshots: async (
    flowId: string,
  ): Promise<{snapshots: {id: string; label: string; createdAt: string; bytes: number}[]}> =>
    requestValidated(`/api/flow/snapshots${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, await getSnapshotsListSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  restoreSnapshot: (flowId: string, snapshotId: string): Promise<{document: FlowDocument}> =>
    request('/api/flow/snapshots/restore', {body: {flowId, snapshotId}}),

  // Property editing + reordering (R3-2): targeted in-line property replaces
  // (other properties' text untouched) and sibling moves. Same undo/parse-gate
  // guarantees as the other block edits.
  updateBlockProperties: (
    flowId: string,
    blockId: string,
    changes: Record<string, string>,
  ): Promise<{document: FlowDocument}> => request('/api/flow/block/properties', {body: {flowId, blockId, changes}}),

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

  // removeBlocks bulk-deletes in ONE server-side patch.
  removeBlocks: (flowId: string, blockIds: string[]): Promise<{document: FlowDocument}> =>
    request('/api/flow/block/remove-batch', {body: {flowId, blockIds}}),

  // renameBlock renames LABEL/COMMENT blocks; labels rewrite their GOTO refs.
  renameBlock: (
    flowId: string,
    blockId: string,
    name: string,
  ): Promise<{document: FlowDocument; gotoRefsUpdated: number}> =>
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
  getSourceMeta: async (flowId: string): Promise<{size: number; modTime: string; files: number}> =>
    requestValidated(`/api/flow/source-meta${flowId ? '?flowId=' + encodeURIComponent(flowId) : ''}`, await getSourceMetaSchema(), {
      method: 'GET',
    }),

  // getSource returns the raw PAD source text (desktop: file; cloud: stored).
  // `file` selects the member of a desktop FOLDER flow (bare filename; the
  // server defaults to Main.txt) — without it the editor showed and saved
  // Main.txt whichever file was selected in the sidebar.
  getSource: (flowId: string, file?: string): Promise<{source: string}> => {
    const params = new URLSearchParams()
    if (flowId) params.set('flowId', flowId)
    if (file) params.set('file', file)
    const qs = params.toString()
    return request(`/api/flow/source${qs ? '?' + qs : ''}`, {method: 'GET'})
  },

  // saveSource replaces the raw source (of `file`, for folder flows),
  // re-parses, and returns the updated doc.
  saveSource: (flowId: string, source: string, file?: string): Promise<FlowDocument> =>
    request('/api/flow/save-source', {body: {flowId, source, file}}),

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
