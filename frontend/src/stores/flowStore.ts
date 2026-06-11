import {create} from 'zustand'
import type {FlowDocument, Block, Subflow, BlockType, FlowFileInfo} from '@/types/domain'
import {useEditorStore} from './editorStore'

function findBlockById(doc: FlowDocument, blockId: string): Block | null {
  for (const subflow of doc.subflows) {
    const found = findBlockInTree(subflow.blocks, blockId)
    if (found) return found
  }
  return null
}

function findSubflowIdByBlock(doc: FlowDocument, blockId: string): string | null {
  for (const subflow of doc.subflows) {
    if (findBlockInTree(subflow.blocks, blockId)) return subflow.id
  }
  return null
}

function findBlockInTree(blocks: Block[], id: string): Block | null {
    for (const block of blocks) {
        if (block.id === id) return block
        if (block.children.length > 0) {
            const found = findBlockInTree(block.children, id)
            if (found) return found
        }
    }
    return null
}

// findAncestorIds returns the container ids on the path to blockId (not the
// block itself), used to un-collapse ancestors when jumping to a nested block.
function findAncestorIds(doc: FlowDocument, blockId: string): string[] {
    const path: string[] = []
    const walk = (blocks: Block[]): boolean => {
        for (const block of blocks) {
            if (block.id === blockId) return true
            if (block.children.length > 0) {
                path.push(block.id)
                if (walk(block.children)) return true
                path.pop()
            }
        }
        return false
    }
    for (const subflow of doc.subflows) {
        path.length = 0
        if (walk(subflow.blocks)) return [...path]
    }
    return []
}

function findLabelBlockInTree(blocks: Block[], labelName: string): Block | null {
    for (const block of blocks) {
        if (block.rawType === 'LABEL' && block.name === labelName) return block
        if (block.children.length > 0) {
            const found = findLabelBlockInTree(block.children, labelName)
            if (found) return found
        }
    }
    return null
}

export const ALL_TYPES: BlockType[] = ['ACTION', 'LOOP', 'CONDITION', 'SUBFLOW', 'ERROR_HANDLER', 'COMMENT', 'VARIABLE', 'WAIT', 'BLOCK', 'SWITCH', 'ELSE', 'CASE', 'DEFAULT', 'END', 'UNKNOWN']

interface FlowState {
  document: FlowDocument | null
  selectedBlockId: string | null
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

  navigationHistory: {blockId: string | null, subflowId: string | null}[]
  historyIndex: number

  setDocument: (doc: FlowDocument | null) => void
  setVisibleBlockId: (id: string | null) => void
  selectBlock: (blockId: string | null, skipHistory?: boolean) => void
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

  reset: () => void

  selectedBlock: () => Block | null
  selectedSubflow: () => Subflow | null
}

export const useFlowStore = create<FlowState>((set, get) => ({
  document: null,
  selectedBlockId: null,
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
  navigationHistory: [],
  historyIndex: -1,

  setDocument: (doc) => {
    const firstId = doc?.subflows[0]?.id ?? null
    set({
      document: doc,
      selectedBlockId: null,
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

    if (firstId) {
      useEditorStore.getState().openInGroup(firstId, 0)
    } else {
      useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})
    }
  },

  setVisibleBlockId: (id) => {
    if (get().visibleBlockId === id) return
    set({visibleBlockId: id})
  },

  selectBlock: (blockId, skipHistory = false) => {
    const state = get()
    if (!blockId || !state.document) {
      set({selectedBlockId: blockId})
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
      expandedBlockIds: nextExpanded
    })

    useEditorStore.getState().openInGroup(subflowId)
  },

  selectSubflow: (subflowId, skipHistory = false) => {
    if (!subflowId) return
    
    if (!skipHistory) {
        const state = get()
        const current = state.navigationHistory[state.historyIndex]
        if (!current || current.subflowId !== subflowId || current.blockId !== null) {
            const nextHistory = [...state.navigationHistory.slice(0, state.historyIndex + 1), {blockId: null, subflowId}].slice(-50)
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

  drillIntoSubflow: (subflowId) => set(state => ({
    drilledSubflowPath: [...state.drilledSubflowPath, subflowId],
    selectedSubflowId: subflowId,
  })),

  drillUp: () => set(state => {
    const path = state.drilledSubflowPath.slice(0, -1)
    return {drilledSubflowPath: path, selectedSubflowId: path[path.length - 1] ?? null}
  }),

  toggleSubflowExpand: (id) => set(state => {
    const next = new Set(state.expandedSubflowIds)
    if (next.has(id)) { next.delete(id) } else { next.add(id) }
    return {expandedSubflowIds: next}
  }),

  toggleBlockExpand: (id) => set(state => {
    const next = new Set(state.expandedBlockIds)
    if (next.has(id)) { next.delete(id) } else { next.add(id) }
    return {expandedBlockIds: next}
  }),

  setVisibleTypes: (types) => set({visibleTypes: types}),
  setParsing: (parsing) => set({isParsing: parsing}),
  setParseError: (error) => set({parseError: error, isParsing: false}),
  setFolderFiles: (files) => set({folderFiles: files, selectedFilePath: null}),
  setSelectedFilePath: (path) => set({selectedFilePath: path}),

  navigateToSubflowByName: (name) => {
    const doc = get().document
    if (!doc) return
    const sf = doc.subflows.find(s => s.name === name)
    if (sf) {
      get().selectSubflow(sf.id)
    }
  },

  navigateToBlock: (blockId) => {
    get().selectBlock(blockId)
  },

  navigateToLabelByName: (labelName) => {
    const doc = get().document
    if (!doc) return
    for (const sf of doc.subflows) {
      const label = findLabelBlockInTree(sf.blocks, labelName)
      if (label) {
        get().selectBlock(label.id)
        return
      }
    }
  },

  reset: () => {
    set({
      document: null, selectedBlockId: null, selectedSubflowId: null,
      drilledSubflowPath: [], expandedSubflowIds: new Set(),
      expandedBlockIds: new Set(), parseError: null,
      folderFiles: null, selectedFilePath: null,
    })
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0, groupWidths: [100]})
  },

  selectedBlock: () => {
    const {document, selectedBlockId} = get()
    if (!document || !selectedBlockId) return null
    return findBlockById(document, selectedBlockId)
  },

  selectedSubflow: () => {
    const {document, selectedSubflowId} = get()
    return document?.subflows.find(s => s.id === selectedSubflowId) ?? null
  },
}))
