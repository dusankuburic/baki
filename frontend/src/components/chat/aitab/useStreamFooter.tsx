import {useMemo} from 'react'
import {MessageBubble, FixProposalCard} from '..'
import LiveToolTrail from '../LiveToolTrail'
import StreamingBubble from '../StreamingBubble'
import type {ChatStreamEngine} from '../hooks/useChatStreamEngine'
import type {FixProposalCard as FixProposalCardState} from '@/stores/chatStore'

// The footer of the message list while a reply streams in: the thinking
// bubble, the streaming indicator, the live tool trail and the approval
// cards. Stable across renders that change none of its inputs, so the
// memoised list (ChatMessageList) skips them.
export function useStreamFooter(deps: {
  showThinking: boolean
  isCurrentThreadStreaming: boolean
  fixProposals: FixProposalCardState[]
  activeThreadId: string | null
  respondFixProposal: ChatStreamEngine['respondFixProposal']
}) {
  const {showThinking, isCurrentThreadStreaming, fixProposals, activeThreadId, respondFixProposal} = deps
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
          fixProposals.map(card => (
            <FixProposalCard
              key={card.proposalId}
              proposal={card}
              onRespond={(approved, proposalId, excludedItemIndices) =>
                respondFixProposal(activeThreadId, approved, proposalId, excludedItemIndices)
              }
            />
          ))}
      </>
    ),
    [showThinking, isCurrentThreadStreaming, fixProposals, activeThreadId, respondFixProposal],
  )
}
