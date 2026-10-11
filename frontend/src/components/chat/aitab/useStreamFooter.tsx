import {useMemo} from 'react'
import {MessageBubble, ApprovalCard} from '..'
import LiveToolTrail from '../LiveToolTrail'
import StreamingBubble from '../StreamingBubble'
import type {ChatStreamEngine} from '../hooks/useChatStreamEngine'
import type {ApprovalCard as ApprovalCardState} from '@/stores/chatStore'

export function useStreamFooter(deps: {
  showThinking: boolean
  isCurrentThreadStreaming: boolean
  approvals: ApprovalCardState[]
  activeThreadId: string | null
  respondFixProposal: ChatStreamEngine['respondFixProposal']
}) {
  const {showThinking, isCurrentThreadStreaming, approvals, activeThreadId, respondFixProposal} = deps
  return useMemo(
    () => (
      <>
        {showThinking && (
          <MessageBubble
            message={{id: 'thinking', role: 'assistant', content: '', timestamp: new Date().toISOString()}}
            isThinking
          />
        )}
        {isCurrentThreadStreaming && <StreamingBubble />}
        {isCurrentThreadStreaming && <LiveToolTrail />}
        {isCurrentThreadStreaming &&
          activeThreadId &&
          approvals.map(card => (
            <ApprovalCard
              key={card.proposalId}
              approval={card}
              onRespond={(approved, proposalId, excludedItemIndices) =>
                respondFixProposal(activeThreadId, approved, proposalId, excludedItemIndices)
              }
            />
          ))}
      </>
    ),
    [showThinking, isCurrentThreadStreaming, approvals, activeThreadId, respondFixProposal],
  )
}
