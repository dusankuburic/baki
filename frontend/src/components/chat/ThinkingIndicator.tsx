import {useTranslation} from 'react-i18next'
import {useEffect, useState} from 'react'
import {Bot} from 'lucide-react'

const SHOW_ELAPSED_AFTER_S = 3

export default function ThinkingIndicator() {
  const {t} = useTranslation('chat')
  const [elapsed, setElapsed] = useState(0)
  useEffect(() => {
    const start = Date.now()
    const id = window.setInterval(() => setElapsed(Math.floor((Date.now() - start) / 1000)), 1000)
    return () => window.clearInterval(id)
  }, [])

  const elapsedLabel =
    elapsed < 60
      ? t('message.elapsedSeconds', {count: elapsed})
      : t('message.elapsedMinutes', {
          minutes: Math.floor(elapsed / 60),
          seconds: String(elapsed % 60).padStart(2, '0'),
        })

  return (
    <div className="flex flex-col items-start gap-1">
      <div className="flex items-center gap-1.5 px-1">
        <Bot size={11} className="text-text-tertiary" />
        <span className="text-2xs font-medium text-text-tertiary">{t('message.roleAssistant')}</span>
      </div>
      <div className="flex items-center gap-2.5 px-4 py-3 bg-surface-2 border border-border-subtle rounded-2xl rounded-tl-md">
        <span className="thinking-dots" aria-hidden="true">
          <span className="thinking-dot" />
          <span className="thinking-dot" />
          <span className="thinking-dot" />
        </span>
        <span role="status" className="thinking-label text-xs">
          {t('message.thinking')}
        </span>
        {elapsed >= SHOW_ELAPSED_AFTER_S && (
          <span className="text-2xs tabular-nums text-text-tertiary/70 animate-fade-in" aria-hidden="true">
            {elapsedLabel}
          </span>
        )}
      </div>
    </div>
  )
}
