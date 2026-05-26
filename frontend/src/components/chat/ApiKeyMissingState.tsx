import {Key} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'
import {Button} from '@/components/shared'

export default function ApiKeyMissingState() {
  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)

  return (
    <div className="flex flex-col items-center justify-center h-full gap-3 px-6 text-center">
      <Key size={24} className="text-text-tertiary" />
      <h3 className="text-sm font-medium text-text-primary">
        Add an API key to start
      </h3>
      <p className="text-xs text-text-tertiary max-w-[240px]">
        Configure an AI provider in Settings to use the AI assistant.
      </p>
      <Button variant="primary" size="sm" onClick={() => setSettingsOpen(true)}>
        Open settings
      </Button>
    </div>
  )
}
