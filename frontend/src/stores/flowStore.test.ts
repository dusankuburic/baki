import {describe, it, expect, beforeEach} from 'vitest'
import {useFlowStore} from './flowStore'
import {useEditorStore} from './editorStore'
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
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})
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
