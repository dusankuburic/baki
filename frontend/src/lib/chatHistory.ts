import {useSettingsStore} from '@/stores/settingsStore'

export function conversationHistoryEnabled(): boolean {
  return useSettingsStore.getState().settings.ai.saveConversationHistory !== false
}
