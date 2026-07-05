import {Key, Sparkles} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import {Button} from '@/components/shared'

export default function ApiKeyMissingState() {
  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)
  const setProvider = useChatStore(s => s.setProvider)

  return (
    <div className="flex flex-col items-center justify-center h-full gap-3 px-6 text-center">
      <Key size={24} className="text-text-tertiary" />
      <h3 className="text-sm font-medium text-text-primary">
        Add an API key to start
      </h3>
      <p className="text-xs text-text-tertiary max-w-[240px]">
        Configure an AI provider in Settings, or try the assistant instantly with demo mode.
      </p>
      <div className="flex items-center gap-2">
        <Button variant="primary" size="sm" onClick={() => setSettingsOpen(true)}>
          Open settings
        </Button>
        <Button variant="ghost" size="sm" onClick={() => setProvider('demo')}>
          <Sparkles size={12} className="mr-1" />
          Try demo mode
        </Button>
      </div>
    </div>
  )
}
