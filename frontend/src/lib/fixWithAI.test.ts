import {describe, it, expect, beforeEach} from 'vitest'
import {stageFindingFix, stageBlockPrompt} from './fixWithAI'
import {useChatStore, threadModelOf} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import type {Finding} from '@/types'

function finding(p: Partial<Finding> = {}): Finding {
  return {
    id: 'F1',
    ruleId: 'hardcoded-credential',
    blockId: 'blk-1',
    title: 'Hardcoded password',
    description: 'A secret is inlined.',
    severity: 'error',
    category: 'security',
    suggestion: 'Use a variable.',
    ...p,
  } as Finding
}

beforeEach(() => {
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    selectedProvider: 'claude',
    stagedPrompt: null,
    drafts: {},
  })
})

describe('stageFindingFix', () => {
  it('creates a dedicated grounded thread and stages the prompt (does not send)', () => {
    stageFindingFix(finding(), 'flow1')
    const st = useChatStore.getState()
    expect(st.threads).toHaveLength(1)
    const thread = st.threads[0]!
    expect(thread.title).toBe('Fix: Hardcoded password')
    expect(thread.contextBlockId).toBe('blk-1')
    expect(thread.useTools).toBe(true)
    expect(st.activeThreadId).toBe(thread.id)
    // Staged for review — the prompt is in stagedPrompt, NOT sent as a message.
    expect(st.stagedPrompt?.threadId).toBe(thread.id)
    expect(st.stagedPrompt?.text).toContain('Hardcoded password')
    expect(st.getMessages(thread.id)).toHaveLength(0)
    expect(useUIStore.getState().inspectorTab).toBe('ai')
  })
})

describe('stageBlockPrompt', () => {
  it('stages into a new thread when none is active', () => {
    stageBlockPrompt('Explain this block', 'blk-9', 'flow1')
    const st = useChatStore.getState()
    expect(st.threads).toHaveLength(1)
    expect(st.threads[0]!.contextBlockId).toBe('blk-9')
    expect(st.stagedPrompt?.text).toBe('Explain this block')
    expect(st.getMessages(st.threads[0]!.id)).toHaveLength(0)
  })

  it('reuses the active thread when one exists', () => {
    const id = useChatStore.getState().createThread('flow1')
    stageBlockPrompt('Explain', 'blk-2', 'flow1')
    const st = useChatStore.getState()
    expect(st.threads).toHaveLength(1)
    expect(st.stagedPrompt?.threadId).toBe(id)
    expect(st.threads[0]!.contextBlockId).toBe('blk-2')
  })
})

// Per-thread model memory: a header pick pins only its own thread, so the
// thread "Fix with AI" opens starts on the Default for new chats — not on the
// docs model another thread was switched to.
describe('Fix with AI and per-thread models', () => {
  it("starts its thread unpinned, never inheriting another thread's pick", () => {
    const docsThread = useChatStore.getState().createThread('flow1')
    useChatStore
      .getState()
      .setThreadModel(docsThread, {provider: 'azure-foundry', model: 'm/docs000000', modelLabel: 'Docs writer'})

    stageFindingFix(finding(), 'flow1')

    const st = useChatStore.getState()
    const fixThread = st.threads.find(t => t.id === st.activeThreadId)!
    expect(fixThread.id).not.toBe(docsThread)
    expect(threadModelOf(fixThread)).toBeNull()
    expect(threadModelOf(st.threads.find(t => t.id === docsThread)!)).toMatchObject({model: 'm/docs000000'})
  })
})
