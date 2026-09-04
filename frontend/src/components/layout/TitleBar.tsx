import {useTranslation} from 'react-i18next'
import {Minus, Square, X, Settings, Menu, PanelRight} from 'lucide-react'
import {createAdapter} from '@/platform/adapters'
import {getPlatformCapabilities} from '@/platform/guards'
import {useBreadcrumbPath} from './Breadcrumbs'
import {useUIStore} from '@/stores/uiStore'
import OrgSwitcher from './OrgSwitcher'
import PresenceIndicators from '@/components/collaboration/PresenceIndicators'
import AlertsBell from './AlertsBell'

const platform = createAdapter()

export default function TitleBar() {
  const {t} = useTranslation('shell')
  const toggleSidebar = useUIStore(s => s.toggleSidebar)
  const toggleInspector = useUIStore(s => s.toggleInspector)

  // Shared derivation (U5a.3): same source as the interactive Breadcrumbs.
  const {flowName, path} = useBreadcrumbPath()
  const breadcrumb = [flowName ?? t('titleBar.appName'), ...path.map(c => c.name)]

  const toggleSettings = useUIStore(s => s.toggleSettings)

  return (
    <div
      className="flex items-center h-8 px-3 border-b border-border-subtle bg-surface-1 flex-shrink-0 print:hidden"
      data-tauri-drag-region
    >
      {/* Mobile sidebar toggle (hamburger) */}
      <button
        onClick={toggleSidebar}
        className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast md:hidden"
        title={t('titleBar.toggleSidebar')}
        aria-label={t('titleBar.toggleSidebar')}
      >
        <Menu size={14} />
      </button>
      <span className="text-xs font-medium text-text-tertiary select-none truncate pointer-events-none">
        {breadcrumb.map((segment, i) => (
          <span key={i}>
            {i > 0 && <span className="text-text-tertiary mx-1">›</span>}
            <span className={i === breadcrumb.length - 1 ? 'text-text-secondary' : 'text-text-tertiary'}>
              {segment}
            </span>
          </span>
        ))}
      </span>
      <div className="flex-1 h-full pointer-events-none" />
      <div className="flex items-center gap-1">
        <PresenceIndicators className="mr-1" />
        {/* Mobile inspector toggle */}
        <button
          onClick={toggleInspector}
          className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast md:hidden"
          title={t('titleBar.toggleInspector')}
          aria-label={t('titleBar.toggleInspector')}
        >
          <PanelRight size={14} />
        </button>
        <OrgSwitcher />
        <AlertsBell />
        <button
          onClick={toggleSettings}
          className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
          title={t('titleBar.settingsTitle')}
          aria-label={t('titleBar.settings')}
        >
          <Settings size={12} />
        </button>
        {getPlatformCapabilities().nativeWindow && (
          <>
            <button
              onClick={() => platform.minimizeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
            >
              <Minus size={10} />
            </button>
            <button
              onClick={() => platform.toggleMaximizeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
            >
              <Square size={8} />
            </button>
            <button
              onClick={() => platform.closeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-semantic-error/15 text-text-tertiary hover:text-semantic-error transition-colors duration-fast"
            >
              <X size={10} />
            </button>
          </>
        )}
      </div>
    </div>
  )
}
