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
  // Fractions of the container width summing to ~1 (not percentages):
  // useEditorGroups' divider-drag math adds pixel-delta/containerWidth to
  // these, so any other scale makes dragging near-inert.
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
    // No-op when nothing would change. selectBlock calls this on EVERY block
    // click, so bailing out here keeps those clicks from churning the groups
    // array and re-rendering every pane; it also terminates the
    // selectSubflow ↔ openInGroup echo (MainPane syncs the focused group's
    // active tab back into the flow store).
    const target = groups[targetIndex]
    if (targetIndex === focusedGroupIndex && target.activeTabId === subflowId && target.tabs.includes(subflowId)) {
      return
    }
    const nextGroups = [...groups]
    const group = {...nextGroups[targetIndex]}

    if (!group.tabs.includes(subflowId)) {
      if (group.tabs.length >= MAX_TABS_PER_GROUP) {
        // At the tab cap — evict the oldest tab so the newly opened subflow
        // still gets a tab. This keeps activeTabId always present in tabs.
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
    const nextGroups = [...groups]
    const group = {...nextGroups[groupIndex]}
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
    // Removing a group left of the focused one shifts the focused group down
    // an index — follow it, otherwise focus silently jumps to its neighbor.
    const nextFocused =
      index < focusedGroupIndex ? focusedGroupIndex - 1 : Math.min(focusedGroupIndex, nextGroups.length - 1)
    set({
      groups: nextGroups,
      focusedGroupIndex: nextFocused,
      groupWidths: nextWidths,
    })
  },

  // pruneToSubflows drops tabs whose subflow doesn't exist in the loaded
  // document — a flow switch (all old ids die) or a reparse that changed
  // subflow ids. Groups that lose every tab are removed so no dead panes
  // remain; when nothing is stale this is a referential no-op, so an
  // apply-fix reload with content-stable ids preserves the split layout.
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

    // Remove from source
    const nextGroups = [...groups]
    const src = {...nextGroups[fromGroup]}
    src.tabs = src.tabs.filter(id => id !== subflowId)
    if (src.activeTabId === subflowId) {
      src.activeTabId = src.tabs[0] || null
    }
    nextGroups[fromGroup] = src

    // Add to dest (same cap/eviction policy as openInGroup)
    const dest = {...nextGroups[toGroup]}
    if (!dest.tabs.includes(subflowId)) {
      dest.tabs =
        dest.tabs.length >= MAX_TABS_PER_GROUP ? [...dest.tabs.slice(1), subflowId] : [...dest.tabs, subflowId]
    }
    dest.activeTabId = subflowId
    nextGroups[toGroup] = dest

    // Moving the last tab out leaves an empty group that renders no tab strip
    // (and therefore no close button) — remove it instead of stranding a dead
    // pane the user can't interact with.
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

// Reset on logout (see storeRegistry).
registerStoreReset(() =>
  useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0, groupWidths: [1]}),
)
