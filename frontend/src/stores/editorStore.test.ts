import {describe, it, expect, beforeEach} from 'vitest'
import {useEditorStore} from './editorStore'

describe('editorStore', () => {
    beforeEach(() => {
        useEditorStore.setState({
            groups: [{tabs: [], activeTabId: null}],
            focusedGroupIndex: 0,
            groupWidths: [100],
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

    it('focusGroup changes focusedGroupIndex', () => {
        useEditorStore.getState().openInGroup('sf1', 0)
        useEditorStore.getState().splitRight()
        useEditorStore.getState().focusGroup(0)
        expect(useEditorStore.getState().focusedGroupIndex).toBe(0)
    })
})
