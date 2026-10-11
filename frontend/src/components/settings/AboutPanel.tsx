import {useTranslation} from 'react-i18next'
import {useSystemStore} from '@/stores/systemStore'

export default function AboutPanel() {
  const {t} = useTranslation('settings')
  const info = useSystemStore(s => s.info)

  return (
    <div className="space-y-4">
      <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
        <div className="text-lg font-bold text-text-primary mb-2">{t('about.productName')}</div>
        <dl className="space-y-1 text-sm text-text-secondary">
          <div className="flex gap-2">
            <dt>{t('about.versionLabel')}:</dt>
            <dd className="text-text-primary">{info?.version ?? '…'}</dd>
          </div>
          <div className="flex gap-2">
            <dt>{t('about.platformLabel')}:</dt>
            <dd className="text-text-primary">{info ? `${info.platform} / ${info.arch}` : '…'}</dd>
          </div>
        </dl>
      </div>

      <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default text-sm text-text-secondary">
        <p className="mb-2">{t('about.blurb')}</p>
        <p className="text-xs text-text-tertiary">{t('about.builtWith')}</p>
      </div>
    </div>
  )
}
