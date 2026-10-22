import {describe, it, expect, beforeEach, afterEach} from 'vitest'
import {useFlowStore, resetDerivedStateForFlow} from './flowStore'
import {useEditorStore} from './editorStore'
import {useSearchStore} from './searchStore'
import {useAnalysisStore} from './analysisStore'
import {useChatStore} from './chatStore'
import {usePresenceStore} from './presenceStore'
import {registerCurrentUserResolver} from './storeRegistry'
import type {Block, FlowDocument, Subflow} from '@/types'

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

beforeEach(() => {
  useFlowStore.getState().reset()
  useEditorStore.setState({groups: [{tabs: [], activeTabId: null}], focusedGroupIndex: 0})
})

describe('resetDerivedStateForFlow', () => {
  it('clears search, analysis, chat thread, and opens first subflow in editor', () => {
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
    expect(useEditorStore.getState().groups[0]!.activeTabId).toBe('sf1')
  })

  it("keeps the flow's own thread active on a same-flow refresh", () => {
    const t = useChatStore.getState().createThread('doc1')
    useChatStore.getState().createThread('doc1')
    useChatStore.getState().switchThread(t)

    resetDerivedStateForFlow(makeDoc(makeSubflow('sf1')))

    expect(useChatStore.getState().activeThreadId).toBe(t)
  })

  it('with a null document clears derived state and empties editor groups', () => {
    useEditorStore.setState({groups: [{tabs: ['sf1'], activeTabId: 'sf1'}], focusedGroupIndex: 0})
    useChatStore.setState({activeThreadId: 't'})

    resetDerivedStateForFlow(null)

    expect(useAnalysisStore.getState().protectedFlowId).toBeNull()
    expect(useChatStore.getState().activeThreadId).toBeNull()
    const groups = useEditorStore.getState().groups
    expect(groups[0]!.tabs).toHaveLength(0)
    expect(groups[0]!.activeTabId).toBeNull()
  })
})

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
    expect(navigationHistory[0]!.subflowId).toBe('sf1')
    expect(historyIndex).toBe(0)
  })
})

describe('applyRemoteDocumentUpdate', () => {
  it('preserves the active chat thread (unlike setDocument)', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1')]))
    useFlowStore.getState().setDocument(doc)
    useChatStore.setState({activeThreadId: 'my-thread'})

    const refreshed = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')]))
    useFlowStore.getState().applyRemoteDocumentUpdate(refreshed)

    expect(useFlowStore.getState().document).toBe(refreshed)
    expect(useChatStore.getState().activeThreadId).toBe('my-thread')
  })

  it('preserves a still-valid block selection, clears an invalid one', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1'), makeBlock('b2', 'sf1')]))
    useFlowStore.getState().setDocument(doc)
    useFlowStore.getState().selectBlock('b1')
    useChatStore.setState({activeThreadId: 't1'})

    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1')])))
    expect(useFlowStore.getState().selectedBlockId).toBe('b1')

    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [])))
    expect(useFlowStore.getState().selectedBlockId).toBeNull()
    expect(useChatStore.getState().activeThreadId).toBe('t1')
  })

  it('keeps chat threads on the same block when a re-parse re-mints the ids, or clears them with a notice', () => {
    // Two blocks of one type and name, so the remap must tell them apart (R-analyzer-boundary-2).
    const call = (id: string, line: number): Block => ({
      ...makeBlock(id, 'sf1'),
      name: 'Invoke Url',
      rawType: 'HTTPClient.InvokeUrl',
      lineNumber: line,
    })
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('a1', 2), call('a2', 3)])))
    const chat = useChatStore.getState()
    const onSecond = chat.createThread('doc1')
    chat.setContextBlock(onSecond, 'a2', useFlowStore.getState().document)
    const onFirst = chat.createThread('doc1')
    chat.setContextBlock(onFirst, 'a1', useFlowStore.getState().document)
    const thread = (id: string) => useChatStore.getState().threads.find(t => t.id === id)!

    // A fix inserted a line above both, moving the second onto the first's old line; the re-parse minted new ids.
    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [call('n1', 3), call('n2', 4)])))
    expect(thread(onSecond).contextBlockId).toBe('n2')
    expect(thread(onFirst).contextBlockId).toBe('n1')

    // A fix removed the second (a duplicate of the first): its thread is cleared and says which block went.
    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [call('m1', 3)])))
    expect(thread(onSecond).contextBlockId).toBeNull()
    expect(thread(onSecond).contextBlockLost).toBe('Invoke Url')
    expect(thread(onFirst).contextBlockId).toBeNull()
  })

  it('prunes editor tabs for removed subflows but keeps the active tab', () => {
    const doc = makeDoc(makeSubflow('sf1'), makeSubflow('sf2'))
    useFlowStore.getState().setDocument(doc)
    useEditorStore.getState().openInGroup('sf2')

    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1')))
    const group = useEditorStore.getState().groups[0]!
    expect(group.tabs).not.toContain('sf2')
  })
})

// Block ids are minted per parse while the desktop flow id is stable, so a thread read back from storage (every
// desktop restart) or kept in the store while another flow was open points at ids the document no longer has.
describe('threads grounded on a block follow it across parses', () => {
  const STORED = 'baki:chatThreads:v2:user-1:doc1'
  const call = (id: string, line: number): Block => ({
    ...makeBlock(id, 'sf1'),
    name: 'Invoke Url',
    rawType: 'HTTPClient.InvokeUrl',
    lineNumber: line,
  })
  const thread = (id: string) => useChatStore.getState().threads.find(t => t.id === id)!
  const emptyChat = () =>
    useChatStore.setState({threads: [], activeThreadId: null, conversations: new Map(), lastActiveByFlow: {}})
  const scopedThread = (blockId: string) => {
    const id = useChatStore.getState().createThread('doc1')
    useChatStore.getState().setContextBlock(id, blockId, useFlowStore.getState().document)
    return id
  }

  beforeEach(() => {
    localStorage.clear()
    registerCurrentUserResolver(() => 'user-1')
    emptyChat()
  })
  afterEach(() => {
    registerCurrentUserResolver(() => null)
    localStorage.clear()
  })

  it('re-anchors a restored thread against the document on screen', () => {
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('a1', 2), call('a2', 3)])))
    const id = scopedThread('a2')

    // A restart: the store is empty and the same file is parsed again with fresh ids before the threads load.
    emptyChat()
    useFlowStore.getState().reset()
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('n1', 2), call('n2', 3)])))
    expect(useChatStore.getState().hydrateThreads('doc1')).toEqual([id])

    expect(thread(id).contextBlockId).toBe('n2')
    expect(thread(id).contextBlockLost).toBeUndefined()
  })

  it('clears a restored thread from before anchors whose id is stale, without naming the block', () => {
    localStorage.setItem(
      STORED,
      JSON.stringify([{id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', flowId: 'doc1', contextBlockId: 'dead-id'}]),
    )
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('n1', 2)])))
    useChatStore.getState().hydrateThreads('doc1')

    const restored = thread('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')
    expect(restored.contextBlockId).toBeNull()
    expect(restored.contextBlockLost).toBe('')
  })

  it('re-anchors the threads already in the store when the flow is opened again after an outside edit', () => {
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('a1', 2), call('a2', 3)])))
    const id = scopedThread('a2')

    useFlowStore.getState().setDocument({...makeDoc(makeSubflow('sf9')), id: 'doc2'})
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('n1', 2), call('n2', 3)])))

    expect(thread(id).contextBlockId).toBe('n2')
  })

  it('keeps the lost-block notice across a reload', () => {
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('a1', 2), call('a2', 3)])))
    const id = scopedThread('a2')
    useFlowStore.getState().applyRemoteDocumentUpdate(makeDoc(makeSubflow('sf1', [call('m1', 2)])))
    expect(thread(id).contextBlockLost).toBe('Invoke Url')

    emptyChat()
    useFlowStore.getState().reset()
    useFlowStore.getState().setDocument(makeDoc(makeSubflow('sf1', [call('m1', 2)])))
    useChatStore.getState().hydrateThreads('doc1')

    expect(thread(id).contextBlockId).toBeNull()
    expect(thread(id).contextBlockLost).toBe('Invoke Url')
  })
})

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
    expect(navigationHistory[1]!.subflowId).toBe('sf2')
  })

  it('selectBlock pushes to history', () => {
    useFlowStore.getState().selectBlock('b1')
    const {navigationHistory, historyIndex} = useFlowStore.getState()
    expect(historyIndex).toBe(1)
    expect(navigationHistory[1]!.blockId).toBe('b1')
  })

  it('selectBlock(null) broadcasts the deselect so collaborators clear the stale selection', () => {
    const doc = makeDoc(makeSubflow('sf1', [makeBlock('b1', 'sf1')]))
    useFlowStore.getState().setDocument(doc)

    const enqueued: string[] = []
    const orig = usePresenceStore.getState().updateSelectedBlock
    usePresenceStore.getState().updateSelectedBlock = (blockId: string) => {
      enqueued.push(blockId)
    }

    try {
      useFlowStore.getState().selectBlock('b1')
      useFlowStore.getState().selectBlock(null)
    } finally {
      usePresenceStore.getState().updateSelectedBlock = orig
    }

    expect(enqueued).toContain('b1')
    expect(enqueued).toContain('')
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
    expect(navigationHistory).toHaveLength(2)
    expect(navigationHistory[1]!.blockId).toBe('b2')
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

describe('selectBlock ancestor expansion', () => {
  it('removes collapsed ancestor containers so the target is visible', () => {
    const child = makeBlock('child', 'sf1')
    const container: Block = {...makeBlock('loop1', 'sf1'), type: 'LOOP', children: [child]}
    const doc = makeDoc(makeSubflow('sf1', [container]))
    useFlowStore.getState().setDocument(doc)

    useFlowStore.getState().toggleBlockExpand('loop1')
    expect(useFlowStore.getState().expandedBlockIds.has('loop1')).toBe(true)

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
