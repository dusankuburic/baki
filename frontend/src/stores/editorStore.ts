import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'

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
  pruneToSubflows: (validSubflowIds: string[]) => void
  moveTabToGroup: (fromGroup: number, subflowId: string, toGroup: number) => void
  setGroupWidths: (widths: number[]) => void
}

export const useEditorStore = create<EditorState>((set, get) => ({
  groups: [{tabs: [], activeTabId: null}],
  focusedGroupIndex: 0,
  groupWidths: [1],

  openInGroup: (subflowId, groupIndex) => {
    const {groups, focusedGroupIndex} = get()
    const targetIndex = groupIndex !== undefined ? groupIndex : focusedGroupIndex
    if (targetIndex < 0 || targetIndex >= groups.length) return
    const target = groups[targetIndex]
    if (!target) return
    if (targetIndex === focusedGroupIndex && target.activeTabId === subflowId && target.tabs.includes(subflowId)) {
      return
    }
    const nextGroups = [...groups]
    const group = {...target}

    if (!group.tabs.includes(subflowId)) {
      if (group.tabs.length >= MAX_TABS_PER_GROUP) {
        group.tabs = [...group.tabs.slice(1), subflowId]
      } else {
        group.tabs = [...group.tabs, subflowId]
      }
    }
    group.activeTabId = subflowId
    nextGroups[targetIndex] = group
    set({groups: nextGroups, focusedGroupIndex: targetIndex})
  },

  closeTab: (groupIndex, subflowId) => {
    const {groups} = get()
    const existing = groups[groupIndex]
    if (!existing) return
    const nextGroups = [...groups]
    const group = {...existing}
    const tabIndex = group.tabs.indexOf(subflowId)
    if (tabIndex === -1) return

    group.tabs = group.tabs.filter(id => id !== subflowId)
    if (group.activeTabId === subflowId) {
      group.activeTabId = group.tabs[Math.max(0, tabIndex - 1)] || null
    }
    nextGroups[groupIndex] = group

    if (group.tabs.length === 0 && nextGroups.length > 1) {
      get().closeGroup(groupIndex)
      return
    }

    set({groups: nextGroups})
  },

  closeAllTabs: groupIndex => {
    const {groups} = get()
    if (groups.length === 1) {
      set({groups: [{tabs: [], activeTabId: null}]})
    } else {
      get().closeGroup(groupIndex)
    }
  },

  closeOtherTabs: (groupIndex, subflowId) => {
    const {groups} = get()
    const existing = groups[groupIndex]
    if (!existing) return
    const nextGroups = [...groups]
    const group = {...existing}
    if (group.tabs.includes(subflowId)) {
      group.tabs = [subflowId]
      group.activeTabId = subflowId
      nextGroups[groupIndex] = group
      set({groups: nextGroups})
    }
  },

  focusGroup: index => set({focusedGroupIndex: index}),

  splitRight: () => {
    const {groups, focusedGroupIndex} = get()
    if (groups.length >= MAX_GROUPS) return

    const activeTabId = groups[focusedGroupIndex]?.activeTabId
    const newGroup = {
      tabs: activeTabId ? [activeTabId] : [],
      activeTabId: activeTabId || null,
    }

    const nextGroups = [...groups, newGroup]
    const nextWidths = nextGroups.map(() => 1 / nextGroups.length)
    set({groups: nextGroups, focusedGroupIndex: nextGroups.length - 1, groupWidths: nextWidths})
  },

  closeGroup: index => {
    const {groups, focusedGroupIndex} = get()
    if (groups.length <= 1) return
    const nextGroups = groups.filter((_, i) => i !== index)
    const nextWidths = nextGroups.map(() => 1 / nextGroups.length)
    const nextFocused =
      index < focusedGroupIndex ? focusedGroupIndex - 1 : Math.min(focusedGroupIndex, nextGroups.length - 1)
    set({
      groups: nextGroups,
      focusedGroupIndex: nextFocused,
      groupWidths: nextWidths,
    })
  },

  pruneToSubflows: validSubflowIds => {
    const valid = new Set(validSubflowIds)
    const {groups, focusedGroupIndex, groupWidths} = get()
    let changed = false
    const pruned = groups.map(g => {
      const tabs = g.tabs.filter(id => valid.has(id))
      if (tabs.length === g.tabs.length && (g.activeTabId === null || valid.has(g.activeTabId))) return g
      changed = true
      return {
        tabs,
        activeTabId: g.activeTabId && valid.has(g.activeTabId) ? g.activeTabId : (tabs[tabs.length - 1] ?? null),
      }
    })
    if (!changed) return
    const nonEmpty = pruned.filter(g => g.tabs.length > 0)
    const nextGroups = nonEmpty.length > 0 ? nonEmpty : [{tabs: [], activeTabId: null}]
    const removedBefore = pruned.slice(0, focusedGroupIndex).filter(g => g.tabs.length === 0).length
    set({
      groups: nextGroups,
      focusedGroupIndex: Math.max(0, Math.min(focusedGroupIndex - removedBefore, nextGroups.length - 1)),
      groupWidths: nextGroups.length === groups.length ? groupWidths : nextGroups.map(() => 1 / nextGroups.length),
    })
  },

  moveTabToGroup: (fromGroup, subflowId, toGroup) => {
    const {groups} = get()
    if (fromGroup === toGroup || toGroup >= groups.length) return

    const nextGroups = [...groups]
    const srcGroup = groups[fromGroup]
    const destGroup = groups[toGroup]
    if (!srcGroup || !destGroup) return
    const src = {...srcGroup}
    src.tabs = src.tabs.filter(id => id !== subflowId)
    if (src.activeTabId === subflowId) {
      src.activeTabId = src.tabs[0] || null
    }
    nextGroups[fromGroup] = src

    const dest = {...destGroup}
    if (!dest.tabs.includes(subflowId)) {
      dest.tabs =
        dest.tabs.length >= MAX_TABS_PER_GROUP ? [...dest.tabs.slice(1), subflowId] : [...dest.tabs, subflowId]
    }
    dest.activeTabId = subflowId
    nextGroups[toGroup] = dest

    if (src.tabs.length === 0) {
      const finalGroups = nextGroups.filter((_, i) => i !== fromGroup)
      set({
        groups: finalGroups,
        focusedGroupIndex: toGroup > fromGroup ? toGroup - 1 : toGroup,
        groupWidths: finalGroups.map(() => 1 / finalGroups.length),
      })
      return
    }

    set({groups: nextGroups, focusedGroupIndex: toGroup})
  },

  setGroupWidths: widths => set({groupWidths: widths}),
}))

registerStoreReset(() =>
  useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0, groupWidths: [1]}),
)
