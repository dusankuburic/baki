import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {formatCompact} from '@/lib/format'

interface Props {
  promptTokens: number
  completionTokens: number
}

interface Seen {
  threadId: string | null
  /** The streaming reply's message id and live count, while one streams. */
  messageId: string | null
  tokens: number
  /** The reply that just ended, until its committed message shows how it ended. */
  awaiting: {messageId: string; tokens: number} | null
  /** The output tokens announced for the reply that last finished on this thread; null for none. */
  ended: number | null
}

// R-frontend-4: the count comes from a store selector (no polling), and it stays out of the live region. The one
// role=status says "Generating answer" while a reply streams, and the reply's output tokens once when it finishes
// with an answer (done's tokensOut, else the live count); a Stop or an error announces no count (the error bubble
// speaks for itself). Its text changes only on start and finish, so assistive tech hears no stream of numbers.
export default function ChatUsageMeter({promptTokens, completionTokens}: Props) {
  const {t} = useTranslation('chat')
  const threadId = useChatStore(s => s.activeThreadId)
  const live = useChatStore(s => (s.activeThreadId ? s.streams[s.activeThreadId]?.tokens : undefined))
  const messageId = useChatStore(s => (s.activeThreadId ? s.streams[s.activeThreadId]?.messageId : undefined)) ?? null
  const isStreaming = live !== undefined
  const tokens = live || 0

  const [seen, setSeen] = useState<Seen>({threadId, messageId, tokens, awaiting: null, ended: null})
  const awaitedId = seen.awaiting?.messageId
  const committed = useChatStore(s =>
    awaitedId && s.activeThreadId ? s.conversations.get(s.activeThreadId)?.find(m => m.id === awaitedId) : undefined,
  )
  if (seen.threadId !== threadId) {
    setSeen({threadId, messageId, tokens, awaiting: null, ended: null})
  } else if (seen.messageId !== messageId) {
    // A reply started or ended: an ended one waits for its committed message; a new one drops the last count.
    const awaiting = seen.messageId ? {messageId: seen.messageId, tokens: seen.tokens} : null
    setSeen({threadId, messageId, tokens, awaiting: messageId ? null : awaiting, ended: null})
  } else if (isStreaming && seen.tokens !== tokens) {
    setSeen({...seen, tokens})
  } else if (seen.awaiting && committed) {
    const answered = committed.finishReason !== 'error' && committed.finishReason !== 'interrupted'
    setSeen({...seen, awaiting: null, ended: answered ? (committed.tokensOut ?? seen.awaiting.tokens) : null})
  }

  const statusText = isStreaming
    ? t('tokens.generating')
    : seen.ended !== null
      ? t('tokens.replyEnded', {count: seen.ended})
      : ''
  return (
    <>
      {/* Always mounted, so its first text is announced as a change of an existing live region. */}
      <span role="status" className="sr-only">
        {statusText}
      </span>
      <MeterFigure
        streaming={isStreaming}
        tokens={tokens}
        promptTokens={promptTokens}
        completionTokens={completionTokens}
      />
    </>
  )
}

function MeterFigure({
  streaming,
  tokens,
  promptTokens,
  completionTokens,
}: Props & {streaming: boolean; tokens: number}) {
  const {t} = useTranslation('chat')
  if (streaming) {
    return (
      <div className="flex items-center gap-2 min-w-0" aria-hidden="true">
        <span className="w-10 h-0.5 rounded-full bg-surface-4 overflow-hidden shrink-0">
          <span className="block w-1/3 h-full rounded-full bg-brand-400 animate-indeterminate" />
        </span>
        <span className="text-2xs text-text-tertiary tabular-nums truncate" title={t('tokens.streamingTitle')}>
          {t('tokens.streamingSoFar', {count: tokens, formatted: formatCompact(tokens)})}
        </span>
      </div>
    )
  }

  const total = promptTokens + completionTokens
  if (total === 0) {
    return <span className="block h-3.5" aria-hidden="true" />
  }

  return (
    <span className="text-2xs text-text-tertiary tabular-nums truncate">
      {t('tokens.total', {formatted: formatCompact(total)})}
      {promptTokens > 0 && completionTokens > 0 && (
        <span className="cq-usage-detail text-text-tertiary/60">
          {' '}
          ({formatCompact(promptTokens)}/{formatCompact(completionTokens)})
        </span>
      )}
    </span>
  )
}
