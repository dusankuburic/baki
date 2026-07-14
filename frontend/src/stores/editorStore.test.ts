import {describe, it, expect, beforeEach} from 'vitest'
import {useEditorStore} from './editorStore'

describe('editorStore', () => {
  beforeEach(() => {
    useEditorStore.setState({
      groups: [{tabs: [], activeTabId: null}],
      focusedGroupIndex: 0,
      groupWidths: [1],
    })
  })

  it('openInGroup adds tab to the group', () => {
    useEditorStore.getState().openInGroup('sf2', 0)
    expect(useEditorStore.getState().groups[0].tabs).toContain('sf2')
  })

  it('openInGroup sets activeTabId', () => {
    useEditorStore.getState().openInGroup('sf2', 0)
    expect(useEditorStore.getState().groups[0].activeTabId).toBe('sf2')
  })

  it('openInGroup does not duplicate tab', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().openInGroup('sf1', 0)
    expect(useEditorStore.getState().groups[0].tabs.filter(t => t === 'sf1')).toHaveLength(1)
  })

  it('closeTab removes the tab', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().openInGroup('sf2', 0)
    useEditorStore.getState().closeTab(0, 'sf2')
    expect(useEditorStore.getState().groups[0].tabs).not.toContain('sf2')
  })

  it('closeTab activates adjacent tab when active tab is closed', () => {
    useEditorStore.getState().openInGroup('sf2', 0)
    useEditorStore.getState().openInGroup('sf1', 0) // sf1 becomes active
    useEditorStore.getState().closeTab(0, 'sf1')
    const {groups} = useEditorStore.getState()
    expect(groups[0].activeTabId).toBe('sf2')
  })

  it('openInGroup is a referential no-op when the tab is already active in the focused group', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    const before = useEditorStore.getState().groups
    // selectBlock calls openInGroup on every block click — re-opening the
    // already-active tab must not churn state (and must not re-render panes).
    useEditorStore.getState().openInGroup('sf1', 0)
    expect(useEditorStore.getState().groups).toBe(before)
  })

  it('splitRight creates a second group', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight()
    expect(useEditorStore.getState().groups).toHaveLength(2)
  })

  it('splitRight focuses the new group', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight()
    expect(useEditorStore.getState().focusedGroupIndex).toBe(1)
  })

  it('closeGroup removes the group', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight()
    useEditorStore.getState().closeGroup(1)
    expect(useEditorStore.getState().groups).toHaveLength(1)
  })

  it('moveTabToGroup moves tab between groups', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().openInGroup('sf2', 0)
    useEditorStore.getState().splitRight()
    // splitRight adds an empty group by default in the new implementation (matching the plan's store)

    useEditorStore.getState().moveTabToGroup(0, 'sf2', 1)
    const {groups} = useEditorStore.getState()
    expect(groups[0].tabs).not.toContain('sf2')
    expect(groups[1].tabs).toContain('sf2')
  })

  it('closeGroup keeps focus on the same group when a group before it closes', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight() // groups: [g0, g1], focused: 1
    useEditorStore.getState().closeGroup(0)
    // g1 shifted down to index 0 — focus must follow it, not point past it.
    expect(useEditorStore.getState().focusedGroupIndex).toBe(0)
    expect(useEditorStore.getState().groups).toHaveLength(1)
  })

  it('moveTabToGroup removes a group emptied by moving its last tab out', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight() // g1 gets sf1 (copied active tab)
    useEditorStore.getState().openInGroup('sf2', 1)
    useEditorStore.getState().closeTab(1, 'sf1') // g1: [sf2]

    useEditorStore.getState().moveTabToGroup(1, 'sf2', 0)
    const {groups, focusedGroupIndex} = useEditorStore.getState()
    // The emptied source group is gone — no dead pane without a tab strip.
    expect(groups).toHaveLength(1)
    expect(groups[0].tabs).toContain('sf2')
    expect(focusedGroupIndex).toBe(0)
  })

  it('pruneToSubflows removes stale tabs and collapses emptied groups', () => {
    // Simulate a flow switch: tabs from the old flow, then a new doc whose
    // subflow ids don't overlap.
    useEditorStore.getState().openInGroup('old1', 0)
    useEditorStore.getState().splitRight() // g1 gets old1
    useEditorStore.getState().openInGroup('old2', 1)

    useEditorStore.getState().pruneToSubflows(['new1', 'new2'])
    const {groups, focusedGroupIndex} = useEditorStore.getState()
    expect(groups).toHaveLength(1)
    expect(groups[0].tabs).toHaveLength(0)
    expect(groups[0].activeTabId).toBeNull()
    expect(focusedGroupIndex).toBe(0)
  })

  it('pruneToSubflows is a referential no-op when all tabs are still valid', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().openInGroup('sf2', 0)
    const before = useEditorStore.getState().groups
    // The apply-fix reload path: subflow ids are content-stable, layout must
    // survive untouched.
    useEditorStore.getState().pruneToSubflows(['sf1', 'sf2', 'sf3'])
    expect(useEditorStore.getState().groups).toBe(before)
  })

  it('pruneToSubflows reactivates a surviving tab when the active one dies', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().openInGroup('gone', 0) // active
    useEditorStore.getState().pruneToSubflows(['sf1'])
    const {groups} = useEditorStore.getState()
    expect(groups[0].tabs).toEqual(['sf1'])
    expect(groups[0].activeTabId).toBe('sf1')
  })

  it('focusGroup changes focusedGroupIndex', () => {
    useEditorStore.getState().openInGroup('sf1', 0)
    useEditorStore.getState().splitRight()
    useEditorStore.getState().focusGroup(0)
    expect(useEditorStore.getState().focusedGroupIndex).toBe(0)
  })
})
