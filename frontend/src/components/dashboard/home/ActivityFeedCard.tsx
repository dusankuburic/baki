import {
  LogIn,
  LogOut,
  Upload,
  Save,
  Trash2,
  Activity as ActivityIcon,
  Share2,
  Bot,
  Key,
  Settings,
  UserPlus,
} from 'lucide-react'
import {CardShell, CardPlaceholder} from './CardShell'
import type {ActivityEntry} from '@/types'

const ACTION_META: Record<string, {icon: typeof LogIn; label: string; color: string}> = {
  'auth.login': {icon: LogIn, label: 'Signed in', color: 'text-emerald-400'},
  'auth.logout': {icon: LogOut, label: 'Signed out', color: 'text-text-tertiary'},
  'auth.register': {icon: UserPlus, label: 'Registered', color: 'text-emerald-400'},
  'auth.sso_login': {icon: LogIn, label: 'SSO login', color: 'text-brand-400'},
  'auth.password_change': {icon: Key, label: 'Changed password', color: 'text-amber-400'},
  'flow.upload': {icon: Upload, label: 'Uploaded flow', color: 'text-brand-400'},
  'flow.save': {icon: Save, label: 'Saved flow', color: 'text-brand-400'},
  'flow.delete': {icon: Trash2, label: 'Deleted flow', color: 'text-red-400'},
  'flow.analyze': {icon: ActivityIcon, label: 'Analyzed flow', color: 'text-cyan-400'},
  'flow.share': {icon: Share2, label: 'Shared flow', color: 'text-purple-400'},
  'chat.stream': {icon: Bot, label: 'AI chat', color: 'text-indigo-400'},
  'keys.save': {icon: Key, label: 'Updated API key', color: 'text-amber-400'},
  'settings.change': {icon: Settings, label: 'Changed settings', color: 'text-text-tertiary'},
  'user.profile_update': {icon: Settings, label: 'Updated profile', color: 'text-text-tertiary'},
}

function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const secs = Math.max(0, (Date.now() - then) / 1000)
  if (secs < 60) return 'just now'
  const mins = secs / 60
  if (mins < 60) return `${Math.floor(mins)}m ago`
  const hrs = mins / 60
  if (hrs < 24) return `${Math.floor(hrs)}h ago`
  const days = hrs / 24
  if (days < 7) return `${Math.floor(days)}d ago`
  return new Date(iso).toLocaleDateString()
}

export function ActivityFeedCard({data, className}: {data: ActivityEntry[]; className?: string}) {
  const hasData = data.length > 0

  return (
    <CardShell title="Recent Activity" className={className}>
      {!hasData ? (
        <CardPlaceholder message="No activity recorded yet." />
      ) : (
        <div className="h-56 overflow-y-auto scrollbar-thin space-y-1 pr-1">
          {data.slice(0, 15).map((entry, i) => {
            const meta = ACTION_META[entry.action] ?? {
              icon: ActivityIcon,
              label: entry.action,
              color: 'text-text-tertiary',
            }
            const Icon = meta.icon
            return (
              <div
                key={`${entry.action}-${i}`}
                className="flex items-center gap-2.5 py-1 px-1 rounded-md hover:bg-surface-3/40 transition-colors"
              >
                <Icon size={13} className={`${meta.color} shrink-0`} />
                <div className="flex-1 min-w-0">
                  <span className="text-xs text-text-primary">{meta.label}</span>
                  {entry.flowName && (
                    <span className="text-xs text-text-tertiary ml-1 truncate">· {entry.flowName}</span>
                  )}
                </div>
                <span className="text-2xs text-text-tertiary tabular-nums shrink-0">
                  {relativeTime(entry.createdAt)}
                </span>
              </div>
            )
          })}
        </div>
      )}
    </CardShell>
  )
}
