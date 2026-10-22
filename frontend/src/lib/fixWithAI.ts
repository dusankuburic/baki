import type {Finding} from '@/types'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'

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

function revealChat() {
  useUIStore.getState().setInspectorTab('ai')
  useUIStore.getState().setInspectorCollapsed(false)
}

export function stageFindingFix(finding: Finding, flowId: string) {
  const store = useChatStore.getState()
  const threadId = store.createThread(flowId)
  store.updateThread(threadId, {title: `Fix: ${finding.title}`, useTools: true})
  store.setContextBlock(threadId, finding.blockId, useFlowStore.getState().document)
  store.setStagedPrompt({threadId, text: buildFindingFixPrompt(finding)})
  store.switchThread(threadId)
  revealChat()
}

export function stageBlockPrompt(text: string, blockId: string, flowId: string) {
  const store = useChatStore.getState()
  let threadId = store.activeThreadId
  if (!threadId) threadId = store.createThread(flowId)
  store.setContextBlock(threadId, blockId, useFlowStore.getState().document)
  store.setStagedPrompt({threadId, text})
  revealChat()
}
