import {useChatStore} from '@/stores/chatStore'
import {useOptionalToast} from '@/components/shared/Toast'
import type {ChatMessage, ProviderID} from '@/types'

// ChatStoreState is the chat store's state shape (chatStore's internal
// ChatState is not part of its facade surface, so it is derived from the
// store hook here — the action types below are picked off it).
export type ChatStoreState = ReturnType<typeof useChatStore.getState>

// StreamMeta is what produced a stream: the provider and model it was sent
// with, and the model's friendly name then (an Azure AI Foundry entry's
// display name — its "m/<id>" means nothing to a reader). Captured at send
// time so the committed message is labelled with the model that actually
// answered, even if the thread's pick changed since.
export interface StreamMeta {
  provider: ProviderID
  model: string
  modelLabel?: string
}

// What an accumulator is primed with: a stream's meta — none for a
// re-attached stream with no record of its turn — and whether the server
// owns the answer's stored copy: a turn sent without history (see send) or
// re-attached without a record of it (see resumeAfterReload). A sent turn
// also keeps what sending it again takes (resend), and whether its one
// automatic retry after a server restart is spent (autoRetried).
export type AccMeta = Partial<StreamMeta> & {serverOwned?: boolean; resend?: TurnResend; autoRetried?: boolean}

// TurnResend is a sent turn as the engine would send it again after a server
// restart cut it short: the question (and the message carrying it, which the
// resend replaces) and how it was sent.
export interface TurnResend {
  text: string
  userMessageId: string
  opts: Pick<SendOptions, 'provider' | 'model' | 'modelLabel' | 'files' | 'excludeContext'>
}

// StreamAcc is one in-flight stream's local accumulation state: the growing
// text plus the RAF batching bookkeeping. Keyed by streamId in a ref so
// multiple threads can stream concurrently; the map entry doubles as the
// stale-event guard — a late event whose streamId has no entry is ignored.
export interface StreamAcc extends AccMeta {
  threadId: string
  text: string
  raf: number | null
  first: boolean
  // linked: the thread's store slot has been seen carrying this streamId.
  // Only a linked acc whose slot later disappears is orphaned (see the store
  // subscription below); before that, the slot may simply not exist yet.
  linked: boolean
}

export interface SendOptions extends StreamMeta {
  files?: string[]
  excludeContext?: boolean
  // Send the thread's (locally truncated) history instead of letting the
  // server replay its stored copy — resend/regenerate.
  includeHistory?: boolean
  // Pin the thread to this provider/model once the send is accepted (its
  // first accepted send is what fixes a new thread's model). False while the
  // thread's own model is only temporarily unavailable and this turn goes to
  // the fallback: the thread keeps its pin and returns to that model once it
  // can be used again. Default true.
  pinThread?: boolean
  // For a turn composed earlier (queued, previewed): the thread's pin then
  // (chatStore.threadPinKey). The send pins only while the thread still has
  // it — a pick made since wins over the captured pair, which is still what
  // this turn goes to.
  pinnedAs?: string
  // A resend (Retry / Regenerate) took the thread's last exchange off to
  // send it again: the thread's messages from before that. A send the
  // server refuses (the model is gone, the per-caller cap) puts them back
  // as they were — the old answer is not lost (and not deleted from the
  // server by the next save), and the question needs no trip back to the
  // composer, which may hold another draft.
  restore?: readonly ChatMessage[]
  // This send is the engine's automatic retry of a turn a server restart
  // (SERVER_RESTARTING) cut short: it gets no automatic retry of its own.
  autoRetry?: boolean
}

// TurnModel is the model half of a send: the thread's pair as resolved when
// the turn was composed (queued, previewed or sent).
export type TurnModel = Pick<SendOptions, 'provider' | 'model' | 'modelLabel' | 'pinThread'>

export type ToastApi = NonNullable<ReturnType<typeof useOptionalToast>>
