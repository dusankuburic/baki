import {useSettingsStore} from '@/stores/settingsStore'

// conversationHistoryEnabled reads "Save conversation history" (Settings → AI
// Behavior) at call time, so flipping it applies to the next turn without
// re-subscribing the chat hooks. The toggle used to have no reader at all:
// every turn was saved whatever it said.
export function conversationHistoryEnabled(): boolean {
  return useSettingsStore.getState().settings.ai.saveConversationHistory !== false
}
