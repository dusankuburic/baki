import {useTranslation} from 'react-i18next'
import {FileText, MessageSquare} from 'lucide-react'

interface Props {
  hasDoc: boolean
  hasThread: boolean
  onCreateThread?: () => void
}

export default function EmptyChatState({hasDoc, hasThread, onCreateThread}: Props) {
  const {t} = useTranslation('chat')
  if (hasDoc && hasThread) return null

  return (
    <div className="flex-1 flex flex-col items-center justify-center px-8 text-center animate-fade-in">
      <div className="flex flex-col items-center gap-4 max-w-sm">
        {!hasDoc ? (
          <>
            <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center">
              <FileText size={24} className="text-text-tertiary" />
            </div>
            <div>
              <h3 className="text-sm font-medium text-text-secondary mb-1">{t('empty.noFlowTitle')}</h3>
              <p className="text-xs text-text-tertiary">{t('empty.noFlowBody')}</p>
            </div>
          </>
        ) : (
          <>
            <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center">
              <MessageSquare size={24} className="text-text-tertiary" />
            </div>
            <div>
              <h3 className="text-sm font-medium text-text-secondary mb-1">{t('empty.noThreadTitle')}</h3>
              <p className="text-xs text-text-tertiary mb-3">{t('empty.noThreadBody')}</p>
              {onCreateThread && (
                <button
                  onClick={onCreateThread}
                  className="px-4 py-2 bg-brand-600 hover:bg-brand-700 text-brand-foreground text-xs font-medium rounded-lg transition-colors"
                >
                  {t('empty.startConversation')}
                </button>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  )
}
