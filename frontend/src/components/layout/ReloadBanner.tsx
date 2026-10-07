import {useTranslation} from 'react-i18next'
import {RefreshCw, X} from 'lucide-react'
import {useBuildVersionStore} from '@/lib/buildVersion'
import {reloadPage} from '@/lib/chunkLoad'

// ReloadBanner offers a reload once the server runs another build than this
// tab's bundle (lib/buildVersion): a tab left open across a deploy runs old
// code against the new API. Non-blocking — a bar in the flow under the title
// bar, never over the work — shown once per session; Dismiss keeps working on
// the bundle as it is.
export default function ReloadBanner() {
  const {t} = useTranslation('shell')
  const offered = useBuildVersionStore(s => s.offered)
  const dismiss = useBuildVersionStore(s => s.dismiss)
  if (!offered) return null

  return (
    <div
      role="status"
      className="flex items-center justify-center gap-3 px-3 py-1.5 border-b border-brand-500/20 bg-brand-500/10 text-xs text-text-primary shrink-0 animate-fade-in print:hidden"
    >
      <RefreshCw size={12} aria-hidden="true" className="shrink-0 text-brand-400" />
      <span>{t('newVersion.message')}</span>
      <button
        onClick={reloadPage}
        className="px-2 py-0.5 rounded-md bg-brand-500 text-brand-foreground font-medium hover:bg-brand-600 transition-colors"
      >
        {t('newVersion.reload')}
      </button>
      <button
        onClick={dismiss}
        title={t('newVersion.dismiss')}
        aria-label={t('newVersion.dismiss')}
        className="w-5 h-5 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors"
      >
        <X size={12} aria-hidden="true" />
      </button>
    </div>
  )
}
