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
  selectedBlockIds: Set<string>
  renamingBlockId: string | null
  visibleBlockId: string | null
  selectedSubflowId: string | null
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
  readOnly: boolean

  navigationHistory: {blockId: string | null; subflowId: string | null}[]
  historyIndex: number

  setDocument: (doc: FlowDocument | null) => void
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
  clearSubflowSelection: () => void
  toggleSubflowExpand: (id: string) => void
  toggleBlockExpand: (id: string) => void
  setVisibleTypes: (types: Set<BlockType>) => void
  setParsing: (parsing: boolean, progress?: number) => void
  setParseError: (error: string | null) => void
  setFolderFiles: (files: FlowFileInfo[] | null) => void
  setSelectedFilePath: (path: string | null) => void

  navigateToSubflowByName: (name: string) => void
  navigateToBlock: (blockId: string) => void
  navigateToLabelByName: (labelName: string) => void
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
    const prevDoc = get().document
    const changedDoc = doc?.id !== prevDoc?.id
    set({
      document: doc,
      ...(changedDoc ? {readOnly: false, libraryFlowId: null, libraryVersion: 0} : {}),
      selectedBlockId: null,
      selectedBlockIds: new Set<string>(),
      renamingBlockId: null,
      visibleBlockId: null,
      selectedSubflowId: firstId,
      expandedSubflowIds: new Set(doc?.subflows.map(s => s.id) ?? []),
      expandedBlockIds: new Set(),
      isParsing: false,
      parseProgress: 0,
      parseError: null,
      navigationHistory: firstId ? [{blockId: null, subflowId: firstId}] : [],
      historyIndex: firstId ? 0 : -1,
    })

    resetDerivedStateForFlow(doc)
    // Every parse mints new block ids, so each incoming document re-anchors the threads grounded on its blocks;
    // after another flow (or none) was open there is no previous parse to name them by, only their anchors.
    if (doc) useChatStore.getState().remapContextBlocks(changedDoc ? null : prevDoc, doc)
  },

  applyRemoteDocumentUpdate: doc => {
    const prev = get()
    const selectedBlockId =
      prev.selectedBlockId && doc && findSubflowIdByBlock(doc, prev.selectedBlockId) ? prev.selectedBlockId : null

    set({
      document: doc,
      selectedBlockId,
      isParsing: false,
      parseProgress: 0,
      parseError: null,
    })

    if (doc) {
      useEditorStore.getState().pruneToSubflows(doc.subflows.map(s => s.id))
      if (prev.document?.id === doc.id) useChatStore.getState().remapContextBlocks(prev.document, doc)
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
      return
    }
    if (!blockId) {
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
      const current = state.navigationHistory[state.historyIndex]
      if (!current || current.blockId !== blockId || current.subflowId !== subflowId) {
        nextHistory = [...state.navigationHistory.slice(0, state.historyIndex + 1), {blockId, subflowId}].slice(-50)
        nextIndex = nextHistory.length - 1
      }
    }

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
    if (!entry) return
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
    if (!entry) return
    set({historyIndex: historyIndex + 1})
    if (entry.blockId) {
      selectBlock(entry.blockId, true)
    } else {
      selectSubflow(entry.subflowId, true)
    }
  },

  clearSubflowSelection: () => set({selectedSubflowId: null}),

  toggleSubflowExpand: id =>
    set(state => ({
      expandedSubflowIds: toggleSetMember(state.expandedSubflowIds, id),
    })),

  toggleBlockExpand: id =>
    set(state => ({
      expandedBlockIds: toggleSetMember(state.expandedBlockIds, id),
    })),

  setVisibleTypes: types => set({visibleTypes: types}),
  setParsing: (parsing, progress = 0) => set({isParsing: parsing, parseProgress: parsing ? progress : 0}),
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

registerStoreReset(() => useFlowStore.getState().reset())

function clearAnalysisState(flowId: string | null) {
  useAnalysisStore.getState().setVariableLineage(null)
  useAnalysisStore.getState().setFindingSearch('')
  useAnalysisStore.getState().setProtectedFlowId(flowId)
  useAnalysisStore.getState().clearFindingSelection()
  useAnalysisStore.getState().setFocusedFinding(null)
}

export function resetDerivedStateForFlow(doc: FlowDocument | null) {
  const firstId = doc?.subflows[0]?.id ?? null
  useSearchStore.getState().clear()
  clearAnalysisState(doc?.id ?? null)
  useChatStore.setState({activeThreadId: doc ? useChatStore.getState().threadForFlow(doc.id) : null})
  if (firstId) {
    useEditorStore.getState().pruneToSubflows(doc?.subflows.map(s => s.id) ?? [])
    useEditorStore.getState().openInGroup(firstId, 0)
  } else {
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})
  }
}
