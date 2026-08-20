import type {Finding} from '@/types'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'

// buildFindingFixPrompt grounds the AI with everything the analyzer knows about
// a finding, including its machine-generated fix hint when present. Shared by
// every "Fix with AI" entry point so the prompt stays identical across them.
function buildFindingFixPrompt(finding: Finding): string {
  return [
    `Help me fix this issue: **${finding.title}**`,
    finding.description,
    finding.suggestion ? `Suggestion: ${finding.suggestion}` : '',
    finding.autoFixHint ? `Analyzer fix hint:\n\`\`\`\n${finding.autoFixHint}\n\`\`\`` : '',
    `Rule: \`${finding.ruleId}\` · Severity: ${finding.severity} · Block: \`${finding.blockId}\``,
  ]
    .filter(Boolean)
    .join('\n\n')
}

// revealChat opens (and un-collapses) the AI tab in the inspector.
function revealChat() {
  useUIStore.getState().setInspectorTab('ai')
  useUIStore.getState().setInspectorCollapsed(false)
}

// stageFindingFix creates a dedicated, grounded thread for a finding and stages
// the fix prompt in its composer for review — the user edits/sends it. Nothing
// is auto-sent (large, tool-enabled prompts should be reviewed before spending
// tokens). Used by every finding-level "Fix with AI" action.
export function stageFindingFix(finding: Finding, flowId: string) {
  const store = useChatStore.getState()
  const threadId = store.createThread(flowId)
  store.updateThread(threadId, {
    title: `Fix: ${finding.title}`,
    contextBlockId: finding.blockId,
    // Fix-with-AI benefits most from grounding, so enable the read-only tool
    // loop for this thread (no-op on providers that don't support tools).
    useTools: true,
  })
  store.setStagedPrompt({threadId, text: buildFindingFixPrompt(finding)})
  store.switchThread(threadId)
  revealChat()
}

// stageBlockPrompt stages a block-scoped prompt ("Explain/Fix with AI" from a
// block) into the active thread's composer, creating a thread if none exists.
// Like stageFindingFix it stages for review rather than auto-sending.
export function stageBlockPrompt(text: string, blockId: string, flowId: string) {
  const store = useChatStore.getState()
  let threadId = store.activeThreadId
  if (!threadId) threadId = store.createThread(flowId)
  store.updateThread(threadId, {contextBlockId: blockId})
  store.setStagedPrompt({threadId, text})
  revealChat()
}
