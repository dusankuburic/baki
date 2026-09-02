import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {FlowDocument, Block, Subflow, BlockType, FlowFileInfo} from '@/types'
import {useEditorStore} from './editorStore'
import {useSearchStore} from './searchStore'
import {useAnalysisStore} from './analysisStore'
import {useChatStore} from './chatStore'
import {usePresenceStore} from './presenceStore'
import {findBlockInDoc, findSubflowIdByBlock, findAncestorIds, findLabelBlock} from '@/lib/tree'
import {toggleSetMember} from '@/lib/collections'

export const ALL_TYPES: BlockType[] = [
  'ACTION',
  'LOOP',
  'CONDITION',
  'SUBFLOW',
  'ERROR_HANDLER',
  'COMMENT',
  'VARIABLE',
  'WAIT',
  'BLOCK',
  'SWITCH',
  'ELSE',
  'CASE',
  'DEFAULT',
  'END',
  'UNKNOWN',
]

// Global document-load generation counter. All code paths that load a document
// call beginDocLoad() before their async fetch and isDocLoadCurrent(gen) after,
// so a stale load from one path can't overwrite a newer load from another.
let docLoadGen = 0
export function beginDocLoad(): number {
  return ++docLoadGen
}
export function isDocLoadCurrent(gen: number): boolean {
  return gen === docLoadGen
}

interface FlowState {
  document: FlowDocument | null
  selectedBlockId: string | null
  // Multi-select (U3b): cmd/ctrl-click toggles, shift-click range-selects.
  // Bulk actions (delete / group move) operate on this set; single-select
  // interactions keep using selectedBlockId as the anchor.
  selectedBlockIds: Set<string>
  // Inline rename target (F2 / double-click on LABEL & COMMENT names).
  renamingBlockId: string | null
  visibleBlockId: string | null
  selectedSubflowId: string | null
  drilledSubflowPath: string[]
  expandedSubflowIds: Set<string>
  expandedBlockIds: Set<string>
  visibleTypes: Set<BlockType>
  isParsing: boolean
  parseProgress: number
  parseError: string | null
  folderFiles: FlowFileInfo[] | null
  selectedFilePath: string | null

  libraryFlowId: string | null
  libraryVersion: number
  // True when the active flow was opened from the cloud library without
  // edit permission (view-only share). Mutation UI (block edit menu, DnD,
  // apply-fix, source save) hides behind this instead of failing on the
  // server after the fact. Same-document refreshes keep the flag; loading a
  // different document resets it (callers like the library open path set it
  // explicitly right after).
  readOnly: boolean

  navigationHistory: {blockId: string | null; subflowId: string | null}[]
  historyIndex: number

  setDocument: (doc: FlowDocument | null) => void
  // Refreshes the document for a SAME-FLOW remote change (collaborator edit)
  // without resetting per-flow UI state. Unlike setDocument, it preserves the
  // active chat thread, selection, search, and editor focus — only the document
  // (and a still-valid selection) is updated.
  applyRemoteDocumentUpdate: (doc: FlowDocument | null) => void
  setVisibleBlockId: (id: string | null) => void
  selectBlock: (blockId: string | null, skipHistory?: boolean) => void
  toggleBlockSelection: (blockId: string) => void
  setBlockSelection: (ids: Set<string>) => void
  clearBlockSelection: () => void
  setRenamingBlock: (blockId: string | null) => void
  selectSubflow: (subflowId: string | null, skipHistory?: boolean) => void
  goBack: () => void
  goForward: () => void
  drillIntoSubflow: (subflowId: string) => void
  drillUp: () => void
  toggleSubflowExpand: (id: string) => void
  toggleBlockExpand: (id: string) => void
  setVisibleTypes: (types: Set<BlockType>) => void
  setParsing: (parsing: boolean) => void
  setParseError: (error: string | null) => void
  setFolderFiles: (files: FlowFileInfo[] | null) => void
  setSelectedFilePath: (path: string | null) => void

  navigateToSubflowByName: (name: string) => void
  navigateToBlock: (blockId: string) => void
  navigateToLabelByName: (labelName: string) => void
  // navigateToSourceFile resolves a source-file mention (e.g. "@Login.txt")
  // to its subflow and selects it. Returns true if a match was found.
  navigateToSourceFile: (fileName: string) => boolean

  reset: () => void

  selectedBlock: () => Block | null
  selectedSubflow: () => Subflow | null
}

export const useFlowStore = create<FlowState>((set, get) => ({
  document: null,
  selectedBlockId: null,
  selectedBlockIds: new Set<string>(),
  renamingBlockId: null,
  visibleBlockId: null,
  selectedSubflowId: null,
  drilledSubflowPath: [],
  expandedSubflowIds: new Set(),
  expandedBlockIds: new Set(),
  visibleTypes: new Set(ALL_TYPES),
  isParsing: false,
  parseProgress: 0,
  parseError: null,
  folderFiles: null,
  selectedFilePath: null,
  libraryFlowId: null,
  libraryVersion: 0,
  readOnly: false,
  navigationHistory: [],
  historyIndex: -1,

  setDocument: doc => {
    const firstId = doc?.subflows[0]?.id ?? null
    // A different document resets read-only (local docs are editable); a
    // same-document refresh (watcher, undo, mutation result) preserves it.
    const changedDoc = doc?.id !== get().document?.id
    set({
      document: doc,
      ...(changedDoc ? {readOnly: false, libraryFlowId: null, libraryVersion: 0} : {}),
      selectedBlockId: null,
      selectedBlockIds: new Set<string>(),
      renamingBlockId: null,
      visibleBlockId: null,
      selectedSubflowId: firstId,
      drilledSubflowPath: firstId ? [firstId] : [],
      expandedSubflowIds: new Set(doc?.subflows.map(s => s.id) ?? []),
      expandedBlockIds: new Set(),
      isParsing: false,
      parseProgress: 0,
      parseError: null,
      navigationHistory: firstId ? [{blockId: null, subflowId: firstId}] : [],
      historyIndex: firstId ? 0 : -1,
    })

    // Clear derived per-flow UI state (search, analysis, chat, editor) via the
    // shared coordinator so the cross-store reset contract lives in one place.
    resetDerivedStateForFlow(doc)
  },

  applyRemoteDocumentUpdate: doc => {
    const prev = get()
    // Preserve the user's block selection if it still exists in the refreshed
    // document; a remote edit may have moved or removed it.
    const selectedBlockId =
      prev.selectedBlockId && doc && findSubflowIdByBlock(doc, prev.selectedBlockId) ? prev.selectedBlockId : null

    set({
      document: doc,
      selectedBlockId,
      isParsing: false,
      parseProgress: 0,
      parseError: null,
    })

    // Drop editor tabs for subflows that no longer exist, but keep the active
    // tab + focus. Do NOT call resetDerivedStateForFlow — same-flow refresh.
    if (doc) {
      useEditorStore.getState().pruneToSubflows(doc.subflows.map(s => s.id))
    }
  },

  setVisibleBlockId: id => {
    if (get().visibleBlockId === id) return
    set({visibleBlockId: id})
  },

  toggleBlockSelection: blockId =>
    set(state => {
      const next = new Set(state.selectedBlockIds)
      if (next.has(blockId)) next.delete(blockId)
      else next.add(blockId)
      return {selectedBlockIds: next}
    }),
  setBlockSelection: ids => set({selectedBlockIds: ids}),
  clearBlockSelection: () => set({selectedBlockIds: new Set<string>()}),
  setRenamingBlock: blockId => set({renamingBlockId: blockId}),

  selectBlock: (blockId, skipHistory = false) => {
    const state = get()
    if (!state.document) {
      set({selectedBlockId: blockId})
      return // no flow loaded → no collaboration to broadcast
    }
    if (!blockId) {
      // Deselect with a flow loaded: broadcast so collaborators stop seeing us
      // on the previous block.
      set({selectedBlockId: null})
      usePresenceStore.getState().updateSelectedBlock('')
      return
    }
    const subflowId = findSubflowIdByBlock(state.document, blockId)
    if (!subflowId) {
      set({selectedBlockId: blockId})
      return
    }

    let nextHistory = state.navigationHistory
    let nextIndex = state.historyIndex

    if (!skipHistory) {
      // Don't push if it's the same as the current entry
      const current = state.navigationHistory[state.historyIndex]
      if (!current || current.blockId !== blockId || current.subflowId !== subflowId) {
        nextHistory = [...state.navigationHistory.slice(0, state.historyIndex + 1), {blockId, subflowId}].slice(-50)
        nextIndex = nextHistory.length - 1
      }
    }

    // Un-collapse any collapsed ancestor containers, otherwise the target is
    // absent from the flattened block list and the jump silently does nothing.
    // (expandedBlockIds is inverted: ids in the set are collapsed.)
    let nextExpanded = state.expandedBlockIds
    const ancestors = findAncestorIds(state.document, blockId)
    if (ancestors.some(id => nextExpanded.has(id))) {
      nextExpanded = new Set(nextExpanded)
      for (const id of ancestors) nextExpanded.delete(id)
    }

    set({
      selectedBlockId: blockId,
      selectedSubflowId: subflowId,
      navigationHistory: nextHistory,
      historyIndex: nextIndex,
      expandedBlockIds: nextExpanded,
    })

    usePresenceStore.getState().updateSelectedBlock(blockId)
    useEditorStore.getState().openInGroup(subflowId)
  },

  selectSubflow: (subflowId, skipHistory = false) => {
    if (!subflowId) return

    if (!skipHistory) {
      const state = get()
      const current = state.navigationHistory[state.historyIndex]
      if (!current || current.subflowId !== subflowId || current.blockId !== null) {
        const nextHistory = [
          ...state.navigationHistory.slice(0, state.historyIndex + 1),
          {blockId: null, subflowId},
        ].slice(-50)
        set({navigationHistory: nextHistory, historyIndex: nextHistory.length - 1})
      }
    }

    set({selectedSubflowId: subflowId})
    useEditorStore.getState().openInGroup(subflowId)
  },

  goBack: () => {
    const {navigationHistory, historyIndex, selectBlock, selectSubflow} = get()
    if (historyIndex <= 0) return
    const entry = navigationHistory[historyIndex - 1]
    set({historyIndex: historyIndex - 1})
    if (entry.blockId) {
      selectBlock(entry.blockId, true)
    } else {
      selectSubflow(entry.subflowId, true)
    }
  },

  goForward: () => {
    const {navigationHistory, historyIndex, selectBlock, selectSubflow} = get()
    if (historyIndex >= navigationHistory.length - 1) return
    const entry = navigationHistory[historyIndex + 1]
    set({historyIndex: historyIndex + 1})
    if (entry.blockId) {
      selectBlock(entry.blockId, true)
    } else {
      selectSubflow(entry.subflowId, true)
    }
  },

  drillIntoSubflow: subflowId =>
    set(state => ({
      drilledSubflowPath: [...state.drilledSubflowPath, subflowId],
      selectedSubflowId: subflowId,
    })),

  drillUp: () =>
    set(state => {
      const path = state.drilledSubflowPath.slice(0, -1)
      return {drilledSubflowPath: path, selectedSubflowId: path[path.length - 1] ?? null}
    }),

  toggleSubflowExpand: id =>
    set(state => ({
      expandedSubflowIds: toggleSetMember(state.expandedSubflowIds, id),
    })),

  toggleBlockExpand: id =>
    set(state => ({
      expandedBlockIds: toggleSetMember(state.expandedBlockIds, id),
    })),

  setVisibleTypes: types => set({visibleTypes: types}),
  setParsing: parsing => set({isParsing: parsing}),
  setParseError: error => set({parseError: error, isParsing: false}),
  setFolderFiles: files => set({folderFiles: files, selectedFilePath: null}),
  setSelectedFilePath: path => set({selectedFilePath: path}),

  navigateToSubflowByName: name => {
    const doc = get().document
    if (!doc) return
    const sf = doc.subflows.find(s => s.name === name)
    if (sf) {
      get().selectSubflow(sf.id)
    }
  },

  navigateToBlock: blockId => {
    get().selectBlock(blockId)
  },

  navigateToSourceFile: fileName => {
    const doc = get().document
    if (!doc) return false
    // Match on the basename so "@dir/Login.txt" and "@Login.txt" both resolve.
    const base = fileName.split(/[/\\]/).pop()?.toLowerCase() ?? ''
    const sf = doc.subflows.find(s => {
      const sfBase = (s.sourceFile ?? '').split(/[/\\]/).pop()?.toLowerCase() ?? ''
      return sfBase === base || s.name.toLowerCase() === base.replace(/\.txt$/, '')
    })
    if (!sf) return false
    get().selectSubflow(sf.id)
    return true
  },

  navigateToLabelByName: labelName => {
    const doc = get().document
    if (!doc) return
    const label = findLabelBlock(doc, labelName)
    if (label) {
      get().selectBlock(label.id)
    }
  },

  reset: () => {
    set({
      document: null,
      readOnly: false,
      libraryFlowId: null,
      libraryVersion: 0,
      selectedBlockId: null,
      selectedBlockIds: new Set<string>(),
      renamingBlockId: null,
      selectedSubflowId: null,
      drilledSubflowPath: [],
      expandedSubflowIds: new Set(),
      expandedBlockIds: new Set(),
      parseError: null,
      folderFiles: null,
      selectedFilePath: null,
      isParsing: false,
      parseProgress: 0,
      visibleBlockId: null,
      visibleTypes: new Set(ALL_TYPES),
      navigationHistory: [],
      historyIndex: -1,
    })
    // Logout teardown. This intentionally differs from resetDerivedStateForFlow
    // (the flow-switch path): it also resets editor group widths, and it leaves
    // the chat activeThreadId alone because the chat store self-resets via its
    // own registerStoreReset handler in the logout cascade.
    useSearchStore.getState().clear()
    clearAnalysisState(null)
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0, groupWidths: [1]})
  },

  selectedBlock: () => {
    const {document, selectedBlockId} = get()
    if (!document || !selectedBlockId) return null
    return findBlockInDoc(document, selectedBlockId)?.block ?? null
  },

  selectedSubflow: () => {
    const {document, selectedSubflowId} = get()
    return document?.subflows.find(s => s.id === selectedSubflowId) ?? null
  },
}))

// Reset on logout (see storeRegistry). reset() also clears search/analysis/editor.
registerStoreReset(() => useFlowStore.getState().reset())

// ---- Cross-store reset coordinator ----
//
// Several stores hold UI state derived from the active document (search,
// analysis, chat, editor groups); it must be cleared on document change so the
// user never sees data belonging to another flow.

// clearAnalysisState resets the analysis store's document-derived fields. Shared
// by the flow-switch coordinator (resetDerivedStateForFlow) and the logout
// teardown (reset).
function clearAnalysisState(flowId: string | null) {
  useAnalysisStore.getState().setVariableLineage(null)
  useAnalysisStore.getState().setFindingSearch('')
  useAnalysisStore.getState().setProtectedFlowId(flowId)
  useAnalysisStore.getState().clearFindingSelection()
  // Clear the finding-focus deep-link so a "finding:" link from flow A doesn't
  // drive a fruitless scroll/highlight against flow B after a switch.
  useAnalysisStore.getState().setFocusedFinding(null)
}

// resetDerivedStateForFlow clears all per-flow UI state in the stores that
// derive from the active document when a flow is loaded or switched. Called by
// setDocument; exported so it can be unit-tested in isolation.
export function resetDerivedStateForFlow(doc: FlowDocument | null) {
  const firstId = doc?.subflows[0]?.id ?? null
  useSearchStore.getState().clear()
  clearAnalysisState(doc?.id ?? null)
  // Clear the active chat thread so the new flow doesn't show the previous
  // flow's conversation; useChatConversations auto-creates one on AITab mount.
  useChatStore.setState({activeThreadId: null})
  if (firstId) {
    // Drop tabs referencing subflows this document doesn't have (flow switch,
    // or a reparse that changed subflow ids) — otherwise old-flow tabs linger
    // invisibly in the strip and split panes keep a dead activeTabId whose
    // pane silently renders fallback content with no tab shown.
    useEditorStore.getState().pruneToSubflows(doc?.subflows.map(s => s.id) ?? [])
    useEditorStore.getState().openInGroup(firstId, 0)
  } else {
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})
  }
}
