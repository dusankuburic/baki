import {useTranslation} from 'react-i18next'
import {Key, Sparkles} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import {Button} from '@/components/shared'

export default function ApiKeyMissingState() {
  const {t} = useTranslation('chat')
  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)
  const setProvider = useChatStore(s => s.setProvider)

  return (
    <div className="flex flex-col items-center justify-center h-full gap-3 px-6 text-center">
      <Key size={24} className="text-text-tertiary" />
      <h3 className="text-sm font-medium text-text-primary">{t('empty.apiKey')}</h3>
      <p className="text-xs text-text-tertiary max-w-[240px]">{t('empty.apiKeyBody')}</p>
      <div className="flex items-center gap-2">
        <Button variant="primary" size="sm" onClick={() => setSettingsOpen(true)}>
          {t('empty.openSettings')}
        </Button>
        <Button variant="ghost" size="sm" onClick={() => setProvider('demo')}>
          <Sparkles size={12} className="mr-1" />
          {t('empty.tryDemo')}
        </Button>
      </div>
    </div>
  )
}
