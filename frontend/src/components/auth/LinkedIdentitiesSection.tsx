import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import React, {useEffect, useState} from 'react'
import {CheckCircle2, KeyRound, Link2} from 'lucide-react'
import {authApi, type IdentityLinkInfo, type SSOInfo} from '@/api/auth'
import {Button, useConfirm, useToast} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {SectionCard, SectionNotice} from './SectionCard'

// readLinkedHash consumes the `#ssoLinked=<provider>` fragment the SSO link
// callback redirects to, and strips it so a reload does not re-announce it.
function readLinkedHash(): string | null {
  const hash = window.location.hash.replace(/^#/, '')
  if (!hash.includes('ssoLinked=')) return null
  const provider = new URLSearchParams(hash).get('ssoLinked')
  history.replaceState(null, '', window.location.pathname + window.location.search)
  return provider
}

// LinkedIdentitiesSection is the "link <provider> from your account settings"
// surface the SSO login error has always pointed at. Until it existed, a user
// who registered by email before SSO was enabled could never use SSO.
export const LinkedIdentitiesSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const {error: toastError, success: toastSuccess} = useToast()
  const {confirm} = useConfirm()
  const [starting, setStarting] = useState(false)
  const [unlinking, setUnlinking] = useState<string | null>(null)

  const {data: sso} = useAsync<SSOInfo | null>(() => authApi.ssoInfo().catch(() => null), [])
  const {data: links, error, refetch} = useAsync<IdentityLinkInfo[]>(() => authApi.ssoLinks().then(l => l ?? []), [])

  useEffect(() => {
    const provider = readLinkedHash()
    if (provider) {
      toastSuccess(t('linked.linkedToast', {provider}))
      refetch()
    }
    // One-shot consumption of an external input on mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (!sso?.enabled) return null
  const provider = sso.provider || 'SSO'
  const linkList = links ?? []
  const current = linkList.find(l => l.provider === provider)

  const handleLink = async () => {
    setStarting(true)
    try {
      const {url} = await authApi.ssoLinkStart()
      window.location.href = url
    } catch (err) {
      setStarting(false)
      toastError(t('linked.linkFailed', {message: getErrorMessage(err)}))
    }
  }

  const handleUnlink = async (link: IdentityLinkInfo) => {
    const ok = await confirm({
      title: t('linked.unlinkTitle', {provider: link.provider}),
      message: t('linked.unlinkMessage', {provider: link.provider}),
      danger: true,
      confirmLabel: t('linked.unlinkConfirm'),
    })
    if (!ok) return
    setUnlinking(link.provider)
    try {
      await authApi.ssoUnlink(link.provider)
      toastSuccess(t('linked.unlinked', {provider: link.provider}))
      refetch()
    } catch (err) {
      toastError(t('linked.unlinkFailed', {message: getErrorMessage(err)}))
    } finally {
      setUnlinking(null)
    }
  }

  return (
    <SectionCard
      id="profile-linked"
      icon={Link2}
      title={t('linked.heading')}
      description={t('linked.body', {provider})}
      action={
        !current ? (
          <Button
            variant="secondary"
            size="sm"
            icon={KeyRound}
            onClick={() => void handleLink()}
            loading={starting}
            disabled={starting}
          >
            {starting ? t('linked.linking') : t('linked.link', {provider})}
          </Button>
        ) : undefined
      }
    >
      {error && (
        <SectionNotice
          message={t('linked.loadFailed')}
          action={
            <Button variant="ghost" size="sm" onClick={refetch}>
              {t('linked.retry')}
            </Button>
          }
        />
      )}
      {linkList.length === 0 && !error && <p className="text-sm text-text-tertiary">{t('linked.none')}</p>}
      {linkList.length > 0 && (
        <ul className="divide-y divide-border-subtle">
          {linkList.map(link => (
            <li key={link.provider} className="flex items-center gap-3 py-2.5 first:pt-0 last:pb-0">
              <span
                className="w-9 h-9 rounded-lg bg-semantic-success/10 text-semantic-success flex items-center justify-center shrink-0"
                aria-hidden="true"
              >
                <CheckCircle2 size={16} />
              </span>
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium text-text-primary flex flex-wrap items-center gap-x-2 gap-y-0.5">
                  <span className="truncate">{link.provider}</span>
                  <span className="px-1.5 py-0.5 rounded text-2xs font-semibold uppercase tracking-wide bg-semantic-success/10 text-semantic-success">
                    {t('linked.linked')}
                  </span>
                </p>
                <p className="text-xs text-text-tertiary truncate">
                  {link.email ? `${link.email} · ` : ''}
                  {t('linked.linkedOn', {date: new Date(link.createdAt).toLocaleDateString()})}
                </p>
              </div>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => void handleUnlink(link)}
                loading={unlinking === link.provider}
                disabled={unlinking !== null}
              >
                {t('linked.unlink')}
              </Button>
            </li>
          ))}
        </ul>
      )}
    </SectionCard>
  )
}
