import {useMemo} from 'react'
import {MessageBubble, ApprovalCard} from '..'
import LiveToolTrail from '../LiveToolTrail'
import StreamingBubble from '../StreamingBubble'
import type {ChatStreamEngine} from '../hooks/useChatStreamEngine'
import type {ApprovalCard as ApprovalCardState} from '@/stores/chatStore'

// The footer of the message list while a reply streams in: the thinking
// bubble, the streaming indicator, the live tool trail and the approval
// cards. Stable across renders that change none of its inputs, so the
// memoised list (ChatMessageList) skips them.
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
        {/* Agentic activity rides INSIDE the scroll region. Pinned below it,
            the tool trail and approval cards grew row by row mid-stream and
            squeezed the conversation as the answer arrived. */}
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
