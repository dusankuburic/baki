import {useTranslation} from 'react-i18next'
import React, {useState} from 'react'
import clsx from 'clsx'
import {HelpCircle, LogOut, Monitor, MonitorSmartphone, Smartphone, Tablet, type LucideIcon} from 'lucide-react'
import {type SessionInfo} from '@/api/auth'
import {Button, IconButton, Skeleton, Spinner, useConfirm, useToast} from '@/components/shared'
import {describeUserAgent, deviceKind, type DeviceKind} from '@/lib/userAgent'
import {absoluteTime, relativeTimeText} from '@/lib/time'
import {useSessions} from './useSessions'
import {SectionCard, SectionNotice} from './SectionCard'

// Five rows answer "is anything here I don't recognise?"; months of sign-ins
// unfold on request instead of pushing the sections below out of view.
const VISIBLE_LIMIT = 5

const DEVICE_ICONS: Record<DeviceKind, LucideIcon> = {
  desktop: Monitor,
  mobile: Smartphone,
  tablet: Tablet,
  unknown: HelpCircle,
}

export const ActiveSessionsSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const toast = useToast()
  const {confirm} = useConfirm()
  const {sessions, loading, error, refetch, revokingId, revoke, currentSessionId, revokingOthers, revokeOthers} =
    useSessions()
  const [showAll, setShowAll] = useState(false)
  // Only offer bulk sign-out when we can tell which session is this one —
  // with a null currentSessionId "others" would include the current session.
  const otherCount = currentSessionId ? sessions.filter(s => s.id !== currentSessionId).length : 0
  const visible = showAll ? sessions : sessions.slice(0, VISIBLE_LIMIT)

  const handleRevoke = async (session: SessionInfo) => {
    const device = describeUserAgent(session.userAgent)
    const ok = await confirm({
      title: t('sessions.signOutDeviceTitle', {device}),
      message: t('sessions.signOutDeviceMessage'),
      confirmLabel: t('sessions.signOutDeviceConfirm'),
      danger: true,
    })
    if (!ok) return
    if (await revoke(session.id)) toast.success(t('sessions.revoked', {device}))
  }

  const handleRevokeOthers = async () => {
    const ok = await confirm({
      title: t('sessions.signOutOthersTitle'),
      message: t('sessions.signOutOthersMessage', {count: otherCount}),
      confirmLabel: t('sessions.signOutOthersConfirm'),
      danger: true,
    })
    if (!ok) return
    const count = await revokeOthers()
    if (count > 0) toast.success(t('sessions.signedOutOthers', {count}))
  }

  return (
    <SectionCard
      id="profile-sessions"
      icon={MonitorSmartphone}
      title={t('sessions.heading')}
      description={t('sessions.description')}
      action={
        otherCount > 0 ? (
          <Button
            variant="secondary"
            size="sm"
            icon={LogOut}
            onClick={() => void handleRevokeOthers()}
            loading={revokingOthers}
          >
            {t('sessions.signOutOthers')}
          </Button>
        ) : undefined
      }
    >
      {error && (
        <SectionNotice
          message={error}
          action={
            <Button variant="ghost" size="sm" onClick={refetch}>
              {t('sessions.retry')}
            </Button>
          }
        />
      )}
      {loading ? (
        <Skeleton lines={3} />
      ) : sessions.length === 0 ? (
        !error && <p className="text-sm text-text-tertiary">{t('sessions.empty')}</p>
      ) : (
        <>
          <ul className="divide-y divide-border-subtle">
            {visible.map(session => (
              <SessionRow
                key={session.id}
                session={session}
                isCurrent={session.id === currentSessionId}
                revoking={revokingId === session.id}
                onRevoke={() => void handleRevoke(session)}
              />
            ))}
          </ul>
          {sessions.length > VISIBLE_LIMIT && (
            <button
              type="button"
              className="mt-3 text-xs font-medium text-brand-400 hover:text-brand-300 transition-colors duration-fast"
              aria-expanded={showAll}
              onClick={() => setShowAll(v => !v)}
            >
              {showAll ? t('sessions.showFewer') : t('sessions.showAll', {count: sessions.length})}
            </button>
          )}
        </>
      )}
    </SectionCard>
  )
}

function SessionRow({
  session,
  isCurrent,
  revoking,
  onRevoke,
}: {
  session: SessionInfo
  isCurrent: boolean
  revoking: boolean
  onRevoke: () => void
}) {
  const {t} = useTranslation('auth')
  const device = describeUserAgent(session.userAgent)
  const Icon = DEVICE_ICONS[deviceKind(session.userAgent)]
  const signedIn = relativeTimeText(session.createdAt)
  const expires = relativeTimeText(session.expiresAt)
  return (
    <li className="profile-session py-3 first:pt-0 last:pb-0" data-current={isCurrent || undefined}>
      <span
        className={clsx(
          'w-9 h-9 rounded-lg flex items-center justify-center',
          isCurrent ? 'bg-semantic-success/10 text-semantic-success' : 'bg-surface-3 text-text-secondary',
        )}
        aria-hidden="true"
      >
        <Icon size={16} />
      </span>
      <div className="min-w-0">
        <p className="text-sm font-medium text-text-primary flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span className="truncate">{device}</span>
          {isCurrent && (
            <span className="px-1.5 py-0.5 rounded text-2xs font-semibold uppercase tracking-wide bg-semantic-success/10 text-semantic-success">
              {t('sessions.thisDevice')}
            </span>
          )}
        </p>
        {signedIn && (
          <p className="text-xs text-text-tertiary mt-0.5 truncate" title={absoluteTime(session.createdAt)}>
            {session.ip
              ? t('sessions.signedInFrom', {when: signedIn, ip: session.ip})
              : t('sessions.signedIn', {when: signedIn})}
          </p>
        )}
        {expires && (
          <p className="text-xs text-text-tertiary truncate" title={absoluteTime(session.expiresAt)}>
            {t('sessions.expires', {when: expires})}
          </p>
        )}
      </div>
      <div className="w-8 flex items-center justify-end">
        {isCurrent ? null : revoking ? (
          <Spinner size={14} />
        ) : (
          <IconButton icon={LogOut} size="md" label={t('sessions.signOutDevice', {device})} onClick={onRevoke} />
        )}
      </div>
    </li>
  )
}
