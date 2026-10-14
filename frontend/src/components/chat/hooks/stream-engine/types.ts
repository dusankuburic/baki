import {useChatStore} from '@/stores/chatStore'
import {useOptionalToast} from '@/components/shared/Toast'
import type {ChatMessage, ProviderID} from '@/types'

export type ChatStoreState = ReturnType<typeof useChatStore.getState>

export interface StreamMeta {
  provider: ProviderID
  model: string
  modelLabel?: string
}

export type AccMeta = Partial<StreamMeta> & {serverOwned?: boolean; resend?: TurnResend; autoRetried?: boolean}

export interface TurnResend {
  text: string
  userMessageId: string
  opts: Pick<SendOptions, 'provider' | 'model' | 'modelLabel' | 'files' | 'excludeContext'>
}

export interface StreamAcc extends AccMeta {
  threadId: string
  text: string
  raf: number | null
  first: boolean
  linked: boolean
}

export interface SendOptions extends StreamMeta {
  files?: string[]
  excludeContext?: boolean
  includeHistory?: boolean
  pinThread?: boolean
  pinnedAs?: string
  restore?: readonly ChatMessage[]
  autoRetry?: boolean
  /** Ask again under this question id: a re-sent turn is the question the server may already hold, not a second one. */
  userMessageId?: string
}

export type TurnModel = Pick<SendOptions, 'provider' | 'model' | 'modelLabel' | 'pinThread'>

export type ToastApi = NonNullable<ReturnType<typeof useOptionalToast>>
