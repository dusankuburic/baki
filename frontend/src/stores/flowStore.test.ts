import {describe, it, expect, beforeEach} from 'vitest'
import {useFlowStore, resetDerivedStateForFlow} from './flowStore'
import {useEditorStore} from './editorStore'
import {useSearchStore} from './searchStore'
import {useAnalysisStore} from './analysisStore'
import {useChatStore} from './chatStore'
import type {Block, FlowDocument, Subflow} from '@/types'

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

// ---- cross-store reset coordinator (F4) ----

describe('resetDerivedStateForFlow', () => {
  it('clears search, analysis, chat thread, and opens first subflow in editor', () => {
    // Seed stale derived state from a "previous" flow.
    useSearchStore.setState({query: 'stale', results: [{id: 'x'} as never]})
    useAnalysisStore.getState().setProtectedFlowId('old-flow')
    useAnalysisStore.getState().setFindingSearch('stale')
    useChatStore.setState({activeThreadId: 'old-thread'})
    useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})

    const doc = makeDoc(makeSubflow('sf1'), makeSubflow('sf2'))
    resetDerivedStateForFlow(doc)

    expect(useSearchStore.getState().query ?? '').toBe('')
    expect(useAnalysisStore.getState().protectedFlowId).toBe('doc1')
    expect(useAnalysisStore.getState().findingSearch).toBe('')
    expect(useChatStore.getState().activeThreadId).toBeNull()
    // First subflow opened in editor group 0.
    expect(useEditorStore.getState().groups[0].activeTabId).toBe('sf1')
  })

  it('with a null document clears derived state and empties editor groups', () => {
    useEditorStore.setState({groups: [{tabs: ['sf1'], activeTabId: 'sf1'}], focusedGroupIndex: 0})
    useChatStore.setState({activeThreadId: 't'})

    resetDerivedStateForFlow(null)

    expect(useAnalysisStore.getState().protectedFlowId).toBeNull()
    expect(useChatStore.getState().activeThreadId).toBeNull()
    const groups = useEditorStore.getState().groups
    expect(groups[0].tabs).toHaveLength(0)
    expect(groups[0].activeTabId).toBeNull()
  })
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

// ---- applyRemoteDocumentUpdate (same-flow remote change) ----

describe('applyRemoteDocumentUpdate', () => {
  it('preserves the active chat thread (unlike setDocument)', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1')]))
    useFlowStore.getState().setDocument(doc)
    useChatStore.setState({activeThreadId: 'my-thread'})

    // A collaborator's save refreshes the same flow's content.
    const refreshed = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')]))
    useFlowStore.getState().applyRemoteDocumentUpdate(refreshed)

    expect(useFlowStore.getState().document).toBe(refreshed)
    // The crux of the fix: the chat thread must survive a remote refresh.
    expect(useChatStore.getState().activeThreadId).toBe('my-thread')
  })

  it('preserves a still-valid block selection, clears an invalid one', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')]))
    useFlowStore.getState().setDocument(doc)
    useFlowStore.getState().selectBlock('b1')
    useChatStore.setState({activeThreadId: 't1'})

    // Remote edit kept b1 → selection preserved.
    useFlowStore
      .getState()
      .applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1')])))
    expect(useFlowStore.getState().selectedBlockId).toBe('b1')

    // Remote edit removed b1 → selection cleared, chat thread still preserved.
    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [])))
    expect(useFlowStore.getState().selectedBlockId).toBeNull()
    expect(useChatStore.getState().activeThreadId).toBe('t1')
  })

  it('prunes editor tabs for removed subflows but keeps the active tab', () => {
    const doc = makeDoc(makeSubflow('sf1'), makeSubflow('sf2'))
    useFlowStore.getState().setDocument(doc) // opens sf1 in group 0
    // Switch the active tab to sf2.
    useEditorStore.getState().openInGroup('sf2')

    // Remote edit removed sf2. The sf2 tab must be pruned.
    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1')))
    const group = useEditorStore.getState().groups[0]
    expect(group.tabs).not.toContain('sf2')
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

// ---- jump-to-block expands collapsed ancestors (F1 regression) ----

describe('selectBlock ancestor expansion', () => {
  it('removes collapsed ancestor containers so the target is visible', () => {
    const child = makeBlock('child', 'sf1')
    const container: Block = {...makeBlock('loop1', 'sf1'), type: 'LOOP', children: [child]}
    const doc = makeDoc(makeSubflow('sf1', [container]))
    useFlowStore.getState().setDocument(doc)

    // User collapses the container (expandedBlockIds is inverted: in-set = collapsed)
    useFlowStore.getState().toggleBlockExpand('loop1')
    expect(useFlowStore.getState().expandedBlockIds.has('loop1')).toBe(true)

    // Jumping to the nested child (e.g. from a finding) must un-collapse it
    useFlowStore.getState().selectBlock('child')
    const state = useFlowStore.getState()
    expect(state.selectedBlockId).toBe('child')
    expect(state.expandedBlockIds.has('loop1')).toBe(false)
  })

  it('leaves unrelated collapsed containers collapsed', () => {
    const child = makeBlock('child', 'sf1')
    const container: Block = {...makeBlock('loop1', 'sf1'), type: 'LOOP', children: [child]}
    const other: Block = {...makeBlock('loop2', 'sf1'), type: 'LOOP', children: [makeBlock('x', 'sf1')]}
    const doc = makeDoc(makeSubflow('sf1', [container, other]))
    useFlowStore.getState().setDocument(doc)

    useFlowStore.getState().toggleBlockExpand('loop1')
    useFlowStore.getState().toggleBlockExpand('loop2')
    useFlowStore.getState().selectBlock('child')

    const state = useFlowStore.getState()
    expect(state.expandedBlockIds.has('loop1')).toBe(false)
    expect(state.expandedBlockIds.has('loop2')).toBe(true)
  })
})

// ---- chat→app navigation (mention deep-links) ----

describe('navigateToSourceFile', () => {
  function docWithSourceFiles(): FlowDocument {
    const main = {...makeSubflow('sf-main'), name: 'Main', sourceFile: 'Main.txt'}
    const login = {...makeSubflow('sf-login'), name: 'Login', sourceFile: 'sub/Login.txt'}
    return makeDoc(main, login)
  }

  it('selects the subflow whose source file matches the mention (basename)', () => {
    useFlowStore.getState().setDocument(docWithSourceFiles())
    const ok = useFlowStore.getState().navigateToSourceFile('sub/Login.txt')
    expect(ok).toBe(true)
    expect(useFlowStore.getState().selectedSubflowId).toBe('sf-login')
  })

  it('matches a bare basename against a nested source file', () => {
    useFlowStore.getState().setDocument(docWithSourceFiles())
    const ok = useFlowStore.getState().navigateToSourceFile('Login.txt')
    expect(ok).toBe(true)
    expect(useFlowStore.getState().selectedSubflowId).toBe('sf-login')
  })

  it('falls back to matching the subflow name without .txt', () => {
    useFlowStore.getState().setDocument(docWithSourceFiles())
    const ok = useFlowStore.getState().navigateToSourceFile('Main.txt')
    expect(ok).toBe(true)
    expect(useFlowStore.getState().selectedSubflowId).toBe('sf-main')
  })

  it('returns false and selects nothing when no file matches', () => {
    useFlowStore.getState().setDocument(docWithSourceFiles())
    const ok = useFlowStore.getState().navigateToSourceFile('Nope.txt')
    expect(ok).toBe(false)
  })
})
