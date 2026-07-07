import {describe, it, expect, beforeEach} from 'vitest'
import {buildFindingFixPrompt, stageFindingFix, stageBlockPrompt} from './fixWithAI'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import type {Finding} from '@/types'

function finding(p: Partial<Finding> = {}): Finding {
  return {
    id: 'F1', ruleId: 'hardcoded-credential', blockId: 'blk-1',
    title: 'Hardcoded password', description: 'A secret is inlined.',
    severity: 'error', category: 'security', suggestion: 'Use a variable.',
    ...p,
  } as Finding
}

beforeEach(() => {
  useChatStore.setState({
    threads: [], activeThreadId: null, conversations: new Map(), streams: {},
    selectedProvider: 'claude', stagedPrompt: null, drafts: {},
  })
})

describe('buildFindingFixPrompt', () => {
  it('includes title, description, suggestion, and rule/severity/block', () => {
    const p = buildFindingFixPrompt(finding({autoFixHint: 'wrap in %var%'}))
    expect(p).toContain('Hardcoded password')
    expect(p).toContain('A secret is inlined.')
    expect(p).toContain('Suggestion: Use a variable.')
    expect(p).toContain('wrap in %var%')
    expect(p).toContain('hardcoded-credential')
  })
})

describe('stageFindingFix', () => {
  it('creates a dedicated grounded thread and stages the prompt (does not send)', () => {
    stageFindingFix(finding(), 'flow1')
    const st = useChatStore.getState()
    expect(st.threads).toHaveLength(1)
    const thread = st.threads[0]
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
    expect(st.threads[0].contextBlockId).toBe('blk-9')
    expect(st.stagedPrompt?.text).toBe('Explain this block')
    expect(st.getMessages(st.threads[0].id)).toHaveLength(0)
  })

  it('reuses the active thread when one exists', () => {
    const id = useChatStore.getState().createThread('flow1')
    stageBlockPrompt('Explain', 'blk-2', 'flow1')
    const st = useChatStore.getState()
    expect(st.threads).toHaveLength(1)
    expect(st.stagedPrompt?.threadId).toBe(id)
    expect(st.threads[0].contextBlockId).toBe('blk-2')
  })
})
