import {create} from 'zustand'

export interface EditorGroup {
  tabs: string[]
  activeTabId: string | null
}

const MAX_GROUPS = 4
const MAX_TABS_PER_GROUP = 8

interface EditorState {
  groups: EditorGroup[]
  focusedGroupIndex: number
  groupWidths: number[]

  openInGroup: (subflowId: string, groupIndex?: number) => void
  closeTab: (groupIndex: number, subflowId: string) => void
  closeAllTabs: (groupIndex: number) => void
  closeOtherTabs: (groupIndex: number, subflowId: string) => void
  focusGroup: (index: number) => void
  splitRight: () => void
  closeGroup: (index: number) => void
  moveTabToGroup: (fromGroup: number, subflowId: string, toGroup: number) => void
  setGroupWidths: (widths: number[]) => void
}

export const useEditorStore = create<EditorState>((set, get) => ({
  groups: [{tabs: [], activeTabId: null}],
  focusedGroupIndex: 0,
  groupWidths: [100],

  openInGroup: (subflowId, groupIndex) => {
    const {groups, focusedGroupIndex} = get()
    const targetIndex = groupIndex !== undefined ? groupIndex : focusedGroupIndex
    const nextGroups = [...groups]
    const group = {...nextGroups[targetIndex]}
    
    if (!group.tabs.includes(subflowId)) {
      if (group.tabs.length >= MAX_TABS_PER_GROUP) return
      group.tabs = [...group.tabs, subflowId]
    }
    group.activeTabId = subflowId
    nextGroups[targetIndex] = group
    set({groups: nextGroups, focusedGroupIndex: targetIndex})
  },

  closeTab: (groupIndex, subflowId) => {
    const {groups} = get()
    const nextGroups = [...groups]
    const group = {...nextGroups[groupIndex]}
    const tabIndex = group.tabs.indexOf(subflowId)
    if (tabIndex === -1) return

    group.tabs = group.tabs.filter(id => id !== subflowId)
    if (group.activeTabId === subflowId) {
      group.activeTabId = group.tabs[Math.max(0, tabIndex - 1)] || null
    }
    nextGroups[groupIndex] = group

    // If a group becomes empty and it's not the only group, close it
    if (group.tabs.length === 0 && nextGroups.length > 1) {
        get().closeGroup(groupIndex)
        return
    }

    set({groups: nextGroups})
  },

  closeAllTabs: (groupIndex) => {
    const {groups} = get()
    if (groups.length === 1) {
      set({groups: [{tabs: [], activeTabId: null}]})
    } else {
      get().closeGroup(groupIndex)
    }
  },

  closeOtherTabs: (groupIndex, subflowId) => {
    const {groups} = get()
    const nextGroups = [...groups]
    const group = {...nextGroups[groupIndex]}
    if (group.tabs.includes(subflowId)) {
      group.tabs = [subflowId]
      group.activeTabId = subflowId
      nextGroups[groupIndex] = group
      set({groups: nextGroups})
    }
  },

  focusGroup: (index) => set({focusedGroupIndex: index}),

  splitRight: () => {
    const {groups} = get()
    if (groups.length >= MAX_GROUPS) return
    const nextGroups = [...groups, {tabs: [], activeTabId: null}]
    const nextWidths = nextGroups.map(() => 100 / nextGroups.length)
    set({groups: nextGroups, focusedGroupIndex: nextGroups.length - 1, groupWidths: nextWidths})
  },

  closeGroup: (index) => {
    const {groups} = get()
    if (groups.length <= 1) return
    const nextGroups = groups.filter((_, i) => i !== index)
    const nextWidths = nextGroups.map(() => 100 / nextGroups.length)
    set({
      groups: nextGroups,
      focusedGroupIndex: Math.min(get().focusedGroupIndex, nextGroups.length - 1),
      groupWidths: nextWidths
    })
  },

  moveTabToGroup: (fromGroup, subflowId, toGroup) => {
    const {groups} = get()
    if (fromGroup === toGroup || toGroup >= groups.length) return
    
    // Remove from source
    const nextGroups = [...groups]
    const src = {...nextGroups[fromGroup]}
    src.tabs = src.tabs.filter(id => id !== subflowId)
    if (src.activeTabId === subflowId) {
      src.activeTabId = src.tabs[0] || null
    }
    nextGroups[fromGroup] = src

    // Add to dest
    const dest = {...nextGroups[toGroup]}
    if (!dest.tabs.includes(subflowId)) {
      dest.tabs = [...dest.tabs, subflowId]
    }
    dest.activeTabId = subflowId
    nextGroups[toGroup] = dest

    set({groups: nextGroups, focusedGroupIndex: toGroup})
  },

  setGroupWidths: (widths) => set({groupWidths: widths}),
}))
