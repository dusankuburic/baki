import {create} from 'zustand'
import type {FlowDocument, Block, Subflow, BlockType, FlowFileInfo} from '@/types/domain'

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

export const ALL_TYPES: BlockType[] = ['ACTION', 'LOOP', 'CONDITION', 'SUBFLOW', 'ERROR_HANDLER', 'COMMENT', 'VARIABLE', 'WAIT', 'BLOCK', 'SWITCH', 'ELSE', 'CASE', 'DEFAULT', 'END', 'UNKNOWN']
const MAX_GROUPS = 4
const MAX_TABS_PER_GROUP = 8

export interface EditorGroup {
  tabs: string[]
  activeTabId: string | null
}

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

  groups: EditorGroup[]
  focusedGroupIndex: number
  groupWidths: number[]

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

  openInGroup: (subflowId: string, groupIndex?: number) => void
  closeTab: (groupIndex: number, subflowId: string) => void
  closeAllTabs: (groupIndex: number) => void
  closeOtherTabs: (groupIndex: number, subflowId: string) => void
  focusGroup: (index: number) => void
  splitRight: () => void
  closeGroup: (index: number) => void
  moveTabToGroup: (fromGroup: number, subflowId: string, toGroup: number) => void
  setGroupWidths: (widths: number[]) => void

  navigateToSubflowByName: (name: string) => void
  navigateToBlock: (blockId: string) => void

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
  groups: [],
  focusedGroupIndex: 0,
  groupWidths: [],
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
      groups: firstId ? [{tabs: [firstId], activeTabId: firstId}] : [],
      focusedGroupIndex: 0,
      groupWidths: [],
      navigationHistory: firstId ? [{blockId: null, subflowId: firstId}] : [],
      historyIndex: firstId ? 0 : -1,
    })
  },

  setVisibleBlockId: (id) => {
    if (get().visibleBlockId === id) return
    set({visibleBlockId: id})
  },

  selectBlock: (blockId, skipHistory = false) => set(state => {
    if (!blockId || !state.document) return {selectedBlockId: blockId}
    const subflowId = findSubflowIdByBlock(state.document, blockId)
    if (!subflowId) return {selectedBlockId: blockId}

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

    const gIdx = state.focusedGroupIndex
    const groups = state.groups.map((g, i) => {
      if (i !== gIdx) return g
      const tabs = g.tabs.includes(subflowId) ? g.tabs : [...g.tabs, subflowId].slice(-MAX_TABS_PER_GROUP)
      return {tabs, activeTabId: subflowId}
    })
    return {
        selectedBlockId: blockId, 
        groups, 
        selectedSubflowId: subflowId,
        navigationHistory: nextHistory,
        historyIndex: nextIndex
    }
  }),

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

    const gIdx = get().focusedGroupIndex
    get().openInGroup(subflowId, gIdx)
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

  openInGroup: (subflowId, groupIndex) => set(state => {
    if (!subflowId) return state
    const gIdx = groupIndex ?? state.focusedGroupIndex
    const groups = state.groups.map((g, i) => {
      if (i !== gIdx) return g
      const tabs = g.tabs.includes(subflowId) ? g.tabs : [...g.tabs, subflowId].slice(-MAX_TABS_PER_GROUP)
      return {tabs, activeTabId: subflowId}
    })
    return {
      groups,
      focusedGroupIndex: gIdx,
      selectedSubflowId: subflowId,
      selectedBlockId: null,
    }
  }),

  closeTab: (groupIndex, subflowId) => set(state => {
    const group = state.groups[groupIndex]
    if (!group) return state
    const tabs = group.tabs.filter(t => t !== subflowId)
    let activeTabId = group.activeTabId
    if (activeTabId === subflowId) {
      const idx = group.tabs.indexOf(subflowId)
      activeTabId = tabs[Math.min(idx, tabs.length - 1)] ?? null
    }
    const groups = state.groups.map((g, i) => i === groupIndex ? {tabs, activeTabId} : g)
    let finalGroups = groups.filter(g => g.tabs.length > 0)
    if (finalGroups.length === 0 && state.document?.subflows[0]) {
      const firstId = state.document.subflows[0].id
      finalGroups = [{tabs: [firstId], activeTabId: firstId}]
    }
    const newFocused = Math.min(state.focusedGroupIndex, finalGroups.length - 1)
    const focusedActive = finalGroups[newFocused]?.activeTabId ?? null
    return {
      groups: finalGroups,
      focusedGroupIndex: Math.max(0, newFocused),
      selectedSubflowId: focusedActive,
      selectedBlockId: null,
      groupWidths: finalGroups.length === state.groups.length ? state.groupWidths : [],
    }
  }),

  focusGroup: (index) => set(state => {
    if (index < 0 || index >= state.groups.length) return state
    const activeTabId = state.groups[index]?.activeTabId ?? null
    return {
      focusedGroupIndex: index,
      selectedSubflowId: activeTabId,
      selectedBlockId: null,
    }
  }),

  splitRight: () => set(state => {
    if (state.groups.length >= MAX_GROUPS) return state
    const fgIdx = state.focusedGroupIndex
    const focusedGroup = state.groups[fgIdx]
    if (!focusedGroup?.activeTabId) return state
    const subflowId = focusedGroup.activeTabId
    const newGroup: EditorGroup = {tabs: [subflowId], activeTabId: subflowId}

    const srcTabs = focusedGroup.tabs.filter(t => t !== subflowId)
    let srcActive = focusedGroup.activeTabId
    if (srcActive === subflowId) {
      const idx = focusedGroup.tabs.indexOf(subflowId)
      srcActive = srcTabs[Math.min(idx, srcTabs.length - 1)] ?? null
    }

    const groups = state.groups.map((g, i) => {
      if (i === fgIdx) return {tabs: srcTabs, activeTabId: srcActive}
      return g
    })
    groups.splice(fgIdx + 1, 0, newGroup)

    let finalGroups = groups.filter(g => g.tabs.length > 0)
    if (finalGroups.length === 0 && state.document?.subflows[0]) {
      const firstId = state.document.subflows[0].id
      finalGroups = [{tabs: [firstId], activeTabId: firstId}]
    }

    const newFocused = Math.min(fgIdx + 1, finalGroups.length - 1)
    return {
      groups: finalGroups,
      focusedGroupIndex: newFocused,
      groupWidths: [],
      selectedSubflowId: subflowId,
      selectedBlockId: null,
    }
  }),

  closeGroup: (index) => set(state => {
    if (state.groups.length <= 1) return state
    const groups = state.groups.filter((_, i) => i !== index)
    const newFocused = Math.min(state.focusedGroupIndex, groups.length - 1)
    const activeTabId = groups[newFocused]?.activeTabId ?? null
    return {
      groups,
      focusedGroupIndex: Math.max(0, newFocused),
      selectedSubflowId: activeTabId,
      selectedBlockId: null,
      groupWidths: [],
    }
  }),

  moveTabToGroup: (fromGroup, subflowId, toGroup) => set(state => {
    if (fromGroup === toGroup) return state
    if (fromGroup < 0 || fromGroup >= state.groups.length) return state
    if (toGroup < 0 || toGroup >= state.groups.length) return state
    const src = state.groups[fromGroup]
    if (!src.tabs.includes(subflowId)) return state

    const srcTabs = src.tabs.filter(t => t !== subflowId)
    let srcActive = src.activeTabId
    if (srcActive === subflowId) {
      const idx = src.tabs.indexOf(subflowId)
      srcActive = srcTabs[Math.min(idx, srcTabs.length - 1)] ?? null
    }

    const dst = state.groups[toGroup]
    const dstTabs = dst.tabs.includes(subflowId) ? dst.tabs : [...dst.tabs, subflowId].slice(-MAX_TABS_PER_GROUP)

    const groups = state.groups.map((g, i) => {
      if (i === fromGroup) return {tabs: srcTabs, activeTabId: srcActive}
      if (i === toGroup) return {tabs: dstTabs, activeTabId: subflowId}
      return g
    })

    let finalGroups = groups.filter(g => g.tabs.length > 0)
    if (finalGroups.length === 0 && state.document?.subflows[0]) {
      const firstId = state.document.subflows[0].id
      finalGroups = [{tabs: [firstId], activeTabId: firstId}]
    }

    const realTo = toGroup < fromGroup ? toGroup : Math.min(toGroup, finalGroups.length - 1)
    return {
      groups: finalGroups,
      focusedGroupIndex: Math.max(0, Math.min(realTo, finalGroups.length - 1)),
      groupWidths: finalGroups.length < groups.length ? [] : state.groupWidths,
      selectedSubflowId: subflowId,
      selectedBlockId: null,
    }
  }),

  closeAllTabs: (groupIndex) => set(state => {
    const group = state.groups[groupIndex]
    if (!group) return state
    
    const groups = state.groups.map((g, i) => i === groupIndex ? {tabs: [], activeTabId: null} : g)
    let finalGroups = groups.filter(g => g.tabs.length > 0)
    
    if (finalGroups.length === 0 && state.document?.subflows[0]) {
      const firstId = state.document.subflows[0].id
      finalGroups = [{tabs: [firstId], activeTabId: firstId}]
    }

    const newFocused = Math.min(state.focusedGroupIndex, finalGroups.length - 1)
    const focusedActive = finalGroups[newFocused]?.activeTabId ?? null
    
    return {
      groups: finalGroups,
      focusedGroupIndex: Math.max(0, newFocused),
      selectedSubflowId: focusedActive,
      selectedBlockId: null,
      groupWidths: finalGroups.length === state.groups.length ? state.groupWidths : [],
    }
  }),

  closeOtherTabs: (groupIndex, subflowId) => set(state => {
    const group = state.groups[groupIndex]
    if (!group || !group.tabs.includes(subflowId)) return state
    
    const groups = state.groups.map((g, i) => 
      i === groupIndex ? {tabs: [subflowId], activeTabId: subflowId} : g
    )
    
    return {
      groups,
      selectedSubflowId: subflowId,
      selectedBlockId: null,
    }
  }),

  setGroupWidths: (widths) => set({groupWidths: widths}),

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

  reset: () => set({
    document: null, selectedBlockId: null, selectedSubflowId: null,
    drilledSubflowPath: [], expandedSubflowIds: new Set(),
    expandedBlockIds: new Set(), parseError: null,
    folderFiles: null, selectedFilePath: null,
    groups: [], focusedGroupIndex: 0, groupWidths: [],
  }),

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
