import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Sparkles, User, Shield, Settings, LayoutDashboard, BarChart3, Cloud, Loader2} from 'lucide-react'
import Button from '@/components/shared/Button'
import Avatar from '@/components/shared/Avatar'
import {useAuthStore} from '@/stores/authStore'
import {useUIStore} from '@/stores/uiStore'

type SidebarToolbarProps = {
  hasFlow: boolean
  isAnalyzing?: boolean
  findingCount?: number
  onAnalyze: () => void
}

export default function SidebarToolbar({hasFlow, isAnalyzing = false, findingCount, onAnalyze}: SidebarToolbarProps) {
  const {t} = useTranslation('shell')
  const user = useAuthStore(s => s.user)
  const mainPaneView = useUIStore(s => s.mainPaneView)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const toggleSettings = useUIStore(s => s.toggleSettings)

  const navClass = (view: 'home' | 'dashboard' | 'library' | 'profile' | 'admin') =>
    clsx(
      'p-1.5 rounded transition-colors',
      mainPaneView === view
        ? 'text-brand-400 bg-brand-500/10'
        : 'text-text-tertiary hover:text-text-primary hover:bg-surface-3',
    )

  return (
    <div className="flex flex-col border-t border-border-subtle bg-surface-1">
      {hasFlow && (
        <div className="p-2 border-b border-border-subtle/50">
          <Button
            variant="primary"
            size="sm"
            fullWidth
            icon={isAnalyzing ? Loader2 : Sparkles}
            onClick={onAnalyze}
            disabled={isAnalyzing}
            className={isAnalyzing ? '[&_svg]:animate-spin' : ''}
          >
            {isAnalyzing ? t('sidebar.analyzing') : t('sidebar.analyzeFlow')}
            {!isAnalyzing && findingCount !== undefined && findingCount > 0 && (
              <span className="ml-1 text-xs opacity-80">{findingCount} findings</span>
            )}
          </Button>
        </div>
      )}

      <div className="flex items-center justify-around h-9 px-2">
        <button
          onClick={() => setMainPaneView('home')}
          className={navClass('home')}
          title={t('sidebar.homeDashboard')}
          aria-label={t('sidebar.homeDashboard')}
        >
          <LayoutDashboard size={16} />
        </button>

        <button
          onClick={() => setMainPaneView('dashboard')}
          className={navClass('dashboard')}
          title={t('sidebar.analyticsDashboard')}
          aria-label={t('sidebar.analyticsDashboard')}
        >
          <BarChart3 size={16} />
        </button>

        <button
          onClick={() => setMainPaneView('library')}
          className={navClass('library')}
          title={t('sidebar.cloudLibrary')}
          aria-label={t('sidebar.cloudLibrary')}
        >
          <Cloud size={16} />
        </button>

        <button
          onClick={() => setMainPaneView('profile')}
          className={clsx(
            'rounded-full transition-colors ring-2',
            mainPaneView === 'profile' ? 'ring-brand-400' : 'ring-transparent hover:ring-border-default',
          )}
          title={t('sidebar.userProfile')}
          aria-label={t('sidebar.userProfile')}
        >
          {user ? (
            <Avatar name={user.displayName || user.email} colorSeed={user.id} avatarUrl={user.avatarUrl} size="sm" />
          ) : (
            <span className={navClass('profile')}>
              <User size={16} />
            </span>
          )}
        </button>

        {user?.role === 'admin' && (
          <button
            onClick={() => setMainPaneView('admin')}
            className={navClass('admin')}
            title={t('sidebar.adminDashboard')}
            aria-label={t('sidebar.adminDashboard')}
          >
            <Shield size={16} />
          </button>
        )}

        <button
          onClick={toggleSettings}
          className="p-1.5 text-text-tertiary hover:text-text-primary hover:bg-surface-3 rounded transition-colors"
          title={t('sidebar.settings')}
          aria-label={t('sidebar.settings')}
        >
          <Settings size={16} />
        </button>
      </div>
    </div>
  )
}
