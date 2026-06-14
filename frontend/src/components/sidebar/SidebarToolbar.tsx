import clsx from 'clsx'
import {Sparkles, User, Shield, Settings, LayoutDashboard, BarChart3} from 'lucide-react'
import Button from '@/components/shared/Button'
import {useAuthStore} from '@/stores/authStore'
import {useUIStore} from '@/stores/uiStore'

type SidebarToolbarProps = {
    hasFlow: boolean
    findingCount?: number
    onAnalyze: () => void
}

export default function SidebarToolbar({hasFlow, findingCount, onAnalyze}: SidebarToolbarProps) {
    const user = useAuthStore(s => s.user)
    const mainPaneView = useUIStore(s => s.mainPaneView)
    const setMainPaneView = useUIStore(s => s.setMainPaneView)
    const toggleSettings = useUIStore(s => s.toggleSettings)

    const navClass = (view: 'home' | 'dashboard' | 'profile' | 'admin') => clsx(
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
                        icon={Sparkles}
                        onClick={onAnalyze}
                    >
                        Analyze flow
                        {findingCount !== undefined && findingCount > 0 && (
                            <span className="ml-1 text-xs opacity-80">{findingCount} findings</span>
                        )}
                    </Button>
                </div>
            )}

            <div className="flex items-center justify-around h-9 px-2">
                <button
                    onClick={() => setMainPaneView('home')}
                    className={navClass('home')}
                    title="Home Dashboard"
                >
                    <LayoutDashboard size={16} />
                </button>

                <button
                    onClick={() => setMainPaneView('dashboard')}
                    className={navClass('dashboard')}
                    title="Analytics Dashboard"
                >
                    <BarChart3 size={16} />
                </button>

                <button
                    onClick={() => setMainPaneView('profile')}
                    className={navClass('profile')}
                    title="User Profile"
                >
                    <User size={16} />
                </button>

                {user?.role === 'admin' && (
                    <button
                        onClick={() => setMainPaneView('admin')}
                        className={navClass('admin')}
                        title="Admin Dashboard"
                    >
                        <Shield size={16} />
                    </button>
                )}

                <button
                    onClick={toggleSettings}
                    className="p-1.5 text-text-tertiary hover:text-text-primary hover:bg-surface-3 rounded transition-colors"
                    title="Settings"
                >
                    <Settings size={16} />
                </button>
            </div>
        </div>
    )
}
