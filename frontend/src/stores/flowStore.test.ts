import {describe, it, expect, beforeEach} from 'vitest'
import {useFlowStore} from './flowStore'
import type {Block, FlowDocument, Subflow} from '@/types/domain'

// ---- helpers ----

function makeBlock(id: string, subflowId = 'sf1'): Block {
    return {
        id,
        name: `Block ${id}`,
        type: 'ACTION',
        rawType: 'ACTION',
        indent: 0,
        lineNumber: 0,
        children: [],
        properties: {},
        variables: [],
        subflowId,
    }
}

function makeSubflow(id: string, blocks: Block[] = []): Subflow {
    return {id, name: `Subflow ${id}`, blocks: blocks.map(b => ({...b, subflowId: id})), variables: []}
}

function makeDoc(...subflows: Subflow[]): FlowDocument {
    return {id: 'doc1', name: 'Test', subflows, variables: [], findingsCount: 0, flows: []} as unknown as FlowDocument
}

// Reset store state before each test
beforeEach(() => {
    useFlowStore.getState().reset()
})

// ---- setDocument ----

describe('setDocument', () => {
    it('selects first subflow when document is loaded', () => {
        const doc = makeDoc(makeSubflow('sf1'), makeSubflow('sf2'))
        useFlowStore.getState().setDocument(doc)
        const state = useFlowStore.getState()
        expect(state.selectedSubflowId).toBe('sf1')
        expect(state.document).toBe(doc)
    })

    it('creates initial group with first subflow tab', () => {
        const doc = makeDoc(makeSubflow('sf1'))
        useFlowStore.getState().setDocument(doc)
        const {groups} = useFlowStore.getState()
        expect(groups).toHaveLength(1)
        expect(groups[0].tabs).toContain('sf1')
        expect(groups[0].activeTabId).toBe('sf1')
    })

    it('resets selection when null is passed', () => {
        const doc = makeDoc(makeSubflow('sf1'))
        useFlowStore.getState().setDocument(doc)
        useFlowStore.getState().setDocument(null)
        const state = useFlowStore.getState()
        expect(state.document).toBeNull()
        expect(state.selectedSubflowId).toBeNull()
    })

    it('initializes navigation history with first subflow entry', () => {
        const doc = makeDoc(makeSubflow('sf1'))
        useFlowStore.getState().setDocument(doc)
        const {navigationHistory, historyIndex} = useFlowStore.getState()
        expect(navigationHistory).toHaveLength(1)
        expect(navigationHistory[0].subflowId).toBe('sf1')
        expect(historyIndex).toBe(0)
    })
})

// ---- navigation history ----

describe('navigation history', () => {
    beforeEach(() => {
        const doc = makeDoc(
            makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')]),
            makeSubflow('sf2', [makeBlock('b3', 'sf2')]),
        )
        useFlowStore.getState().setDocument(doc)
    })

    it('selectSubflow pushes to history', () => {
        useFlowStore.getState().selectSubflow('sf2')
        const {navigationHistory, historyIndex} = useFlowStore.getState()
        expect(historyIndex).toBe(1)
        expect(navigationHistory[1].subflowId).toBe('sf2')
    })

    it('selectBlock pushes to history', () => {
        useFlowStore.getState().selectBlock('b1')
        const {navigationHistory, historyIndex} = useFlowStore.getState()
        expect(historyIndex).toBe(1)
        expect(navigationHistory[1].blockId).toBe('b1')
    })

    it('goBack navigates to previous entry', () => {
        useFlowStore.getState().selectSubflow('sf2')
        useFlowStore.getState().goBack()
        const {historyIndex, selectedSubflowId} = useFlowStore.getState()
        expect(historyIndex).toBe(0)
        expect(selectedSubflowId).toBe('sf1')
    })

    it('goForward navigates to next entry after going back', () => {
        useFlowStore.getState().selectSubflow('sf2')
        useFlowStore.getState().goBack()
        useFlowStore.getState().goForward()
        const {historyIndex, selectedSubflowId} = useFlowStore.getState()
        expect(historyIndex).toBe(1)
        expect(selectedSubflowId).toBe('sf2')
    })

    it('goBack does nothing at the start of history', () => {
        useFlowStore.getState().goBack()
        expect(useFlowStore.getState().historyIndex).toBe(0)
    })

    it('goForward does nothing at the end of history', () => {
        const before = useFlowStore.getState().historyIndex
        useFlowStore.getState().goForward()
        expect(useFlowStore.getState().historyIndex).toBe(before)
    })

    it('new navigation after going back truncates future entries', () => {
        useFlowStore.getState().selectSubflow('sf2')
        useFlowStore.getState().goBack()
        useFlowStore.getState().selectBlock('b2')
        const {navigationHistory} = useFlowStore.getState()
        // sf1 (init) + b2 = 2 entries; sf2 was cut
        expect(navigationHistory).toHaveLength(2)
        expect(navigationHistory[1].blockId).toBe('b2')
    })

    it('skipHistory=true does not push to history', () => {
        useFlowStore.getState().selectBlock('b1', true)
        const {historyIndex} = useFlowStore.getState()
        expect(historyIndex).toBe(0)
    })

    it('selecting the same entry twice does not duplicate history', () => {
        useFlowStore.getState().selectSubflow('sf2')
        useFlowStore.getState().selectSubflow('sf2')
        expect(useFlowStore.getState().navigationHistory).toHaveLength(2)
    })
})

// ---- tab management ----

describe('tab management', () => {
    beforeEach(() => {
        const doc = makeDoc(
            makeSubflow('sf1'),
            makeSubflow('sf2'),
            makeSubflow('sf3'),
        )
        useFlowStore.getState().setDocument(doc)
    })

    it('openInGroup adds tab to the group', () => {
        useFlowStore.getState().openInGroup('sf2', 0)
        expect(useFlowStore.getState().groups[0].tabs).toContain('sf2')
    })

    it('openInGroup sets activeTabId', () => {
        useFlowStore.getState().openInGroup('sf2', 0)
        expect(useFlowStore.getState().groups[0].activeTabId).toBe('sf2')
    })

    it('openInGroup does not duplicate tab', () => {
        useFlowStore.getState().openInGroup('sf1', 0)
        useFlowStore.getState().openInGroup('sf1', 0)
        expect(useFlowStore.getState().groups[0].tabs.filter(t => t === 'sf1')).toHaveLength(1)
    })

    it('closeTab removes the tab', () => {
        useFlowStore.getState().openInGroup('sf2', 0)
        useFlowStore.getState().closeTab(0, 'sf2')
        expect(useFlowStore.getState().groups[0].tabs).not.toContain('sf2')
    })

    it('closeTab activates adjacent tab when active tab is closed', () => {
        useFlowStore.getState().openInGroup('sf2', 0)
        useFlowStore.getState().openInGroup('sf1', 0) // sf1 becomes active
        useFlowStore.getState().closeTab(0, 'sf1')
        const {groups} = useFlowStore.getState()
        // sf2 should become active
        expect(groups[0].activeTabId).toBe('sf2')
    })
})

// ---- group management ----

describe('group management', () => {
    beforeEach(() => {
        const doc = makeDoc(makeSubflow('sf1'), makeSubflow('sf2'))
        useFlowStore.getState().setDocument(doc)
        // Open sf2 so the focused group has 2 tabs — splitRight needs at least 2 tabs
        // to leave the source group non-empty
        useFlowStore.getState().openInGroup('sf2', 0)
        useFlowStore.getState().openInGroup('sf1', 0) // make sf1 active again
    })

    it('splitRight creates a second group', () => {
        useFlowStore.getState().splitRight()
        expect(useFlowStore.getState().groups).toHaveLength(2)
    })

    it('splitRight focuses the new group', () => {
        useFlowStore.getState().splitRight()
        expect(useFlowStore.getState().focusedGroupIndex).toBe(1)
    })

    it('closeGroup removes the group', () => {
        useFlowStore.getState().splitRight()
        useFlowStore.getState().closeGroup(1)
        expect(useFlowStore.getState().groups).toHaveLength(1)
    })

    it('cannot close group when there is only one', () => {
        useFlowStore.getState().closeGroup(0)
        expect(useFlowStore.getState().groups).toHaveLength(1)
    })

    it('moveTabToGroup moves tab between groups', () => {
        useFlowStore.getState().splitRight()
        // After split: group 0 has [sf2], group 1 has [sf1] (sf1 was the active tab moved right)
        // Move sf2 from group 0 to group 1
        useFlowStore.getState().moveTabToGroup(0, 'sf2', 1)
        const {groups} = useFlowStore.getState()
        const allTabs = groups.flatMap(g => g.tabs)
        expect(allTabs).toContain('sf2')
        const g1 = groups.find(g => g.tabs.includes('sf1'))!
        expect(g1.tabs).toContain('sf2')
    })

    it('focusGroup changes focusedGroupIndex and selectedSubflowId', () => {
        useFlowStore.getState().openInGroup('sf2', 0)
        useFlowStore.getState().splitRight()
        useFlowStore.getState().focusGroup(0)
        const state = useFlowStore.getState()
        expect(state.focusedGroupIndex).toBe(0)
    })
})
