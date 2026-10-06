import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {useChatStore} from '@/stores/chatStore'
import {threadModelOf} from '@/stores/chatStore'
import {findModel} from '@/lib/modelIds'
import {resolveThreadModel} from '@/lib/modelSelection'
import type {useProviderSetup} from '../hooks/useProviderSetup'
import type {useModelNames} from './useModelNames'
import type {ChatThread} from '@/stores/chatStore'

type ProviderSetup = ReturnType<typeof useProviderSetup>
type ModelNames = ReturnType<typeof useModelNames>

interface ThreadModelLabelsDeps {
  flowThreads: ChatThread[]
  activeThreadId: string | null
  selection: ProviderSetup['selection']
  selectionSettings: ProviderSetup['settings']
  providers: ProviderSetup['providers']
  listingLoaded: ProviderSetup['listingLoaded']
  purposeOf: ModelNames['purposeOf']
  listedModels: ModelNames['listedModels']
}

// Each tab's tooltip names its thread's model: the active thread's as
// resolved by useProviderSetup, the others' from their pins, else — a thread
// from before per-thread memory — from their own loaded history (an unpinned
// one with none starts on the Default for new chats).
export function useThreadModelLabels({
  flowThreads,
  activeThreadId,
  selection,
  selectionSettings,
  providers,
  listingLoaded,
  purposeOf,
  listedModels,
}: ThreadModelLabelsDeps) {
  const {t} = useTranslation('chat')
  const conversations = useChatStore(s => s.conversations)
  return useMemo(() => {
    const out: Record<string, string> = {}
    for (const th of flowThreads) {
      const sel =
        th.id === activeThreadId
          ? selection
          : resolveThreadModel({
              pin: threadModelOf(th),
              messages: conversations.get(th.id),
              settings: selectionSettings,
              providers,
              listingLoaded,
            })
      if (!sel.label) continue
      const purpose = sel.model ? purposeOf(findModel(sel.model, listedModels(sel.provider))) : undefined
      out[th.id] = purpose ? t('threads.modelPurpose', {model: sel.label, purpose}) : sel.label
    }
    return out
  }, [
    flowThreads,
    activeThreadId,
    selection,
    conversations,
    selectionSettings,
    providers,
    listingLoaded,
    purposeOf,
    listedModels,
    t,
  ])
}
