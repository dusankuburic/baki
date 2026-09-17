import {useTranslation} from 'react-i18next'
import React, {useEffect, useState} from 'react'
import {KeyRound, Link2, XCircle} from 'lucide-react'
import {authApi, type IdentityLinkInfo, type SSOInfo} from '@/api/auth'
import Button from '@/components/shared/Button'
import {useConfirm, useToast} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'

// readLinkedHash consumes the `#ssoLinked=<provider>` fragment the SSO link
// callback redirects to, and strips it so a reload does not re-announce it.
function readLinkedHash(): string | null {
  const hash = window.location.hash.replace(/^#/, '')
  if (!hash.includes('ssoLinked=')) return null
  const provider = new URLSearchParams(hash).get('ssoLinked')
  history.replaceState(null, '', window.location.pathname + window.location.search)
  return provider
}

// LinkedIdentitiesCard is the "link <provider> from your account settings"
// surface the SSO login error has always pointed at. Until it existed, a user
// who registered by email before SSO was enabled could never use SSO.
export const LinkedIdentitiesCard: React.FC = () => {
  const {t} = useTranslation('auth')
  const {error: toastError, success: toastSuccess} = useToast()
  const {confirm} = useConfirm()
  const [starting, setStarting] = useState(false)
  const [unlinking, setUnlinking] = useState<string | null>(null)

  const {data: sso} = useAsync<SSOInfo | null>(() => authApi.ssoInfo().catch(() => null), [])
  const {
    data: links,
    error,
    refetch,
  } = useAsync<IdentityLinkInfo[]>(() => authApi.ssoLinks().then(l => l ?? []), [])

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
      toastError(t('linked.linkFailed', {message: err instanceof Error ? err.message : String(err)}))
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
      toastError(t('linked.unlinkFailed', {message: err instanceof Error ? err.message : String(err)}))
    } finally {
      setUnlinking(null)
    }
  }

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
        <KeyRound size={13} className="text-text-tertiary" />
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">{t('linked.heading')}</h2>
      </div>
      <div className="p-5 space-y-3">
        <p className="text-sm text-text-tertiary">{t('linked.body', {provider})}</p>
        {error && (
          <div className="flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border bg-semantic-error/10 border-semantic-error/30 text-semantic-error">
            <XCircle size={14} className="mt-0.5 flex-shrink-0" />
            <span>{t('linked.loadFailed')}</span>
          </div>
        )}
        {linkList.length === 0 && !error && <p className="text-xs text-text-tertiary">{t('linked.none')}</p>}
        {linkList.map(link => (
          <div
            key={link.provider}
            className="flex items-center justify-between gap-3 rounded-lg border border-border-subtle bg-surface-1 px-3 py-2"
          >
            <div className="min-w-0">
              <p className="text-sm font-medium text-text-primary flex items-center gap-1.5">
                <Link2 size={12} className="text-semantic-success" />
                {link.provider}
                <span className="text-2xs font-normal text-semantic-success">{t('linked.linked')}</span>
              </p>
              <p className="text-xs text-text-tertiary truncate">
                {link.email ? `${link.email} · ` : ''}
                {t('linked.linkedOn', {date: new Date(link.createdAt).toLocaleDateString()})}
              </p>
            </div>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => handleUnlink(link)}
              loading={unlinking === link.provider}
              disabled={unlinking !== null}
            >
              {t('linked.unlink')}
            </Button>
          </div>
        ))}
        {!current && (
          <Button variant="secondary" size="sm" icon={KeyRound} onClick={handleLink} loading={starting} disabled={starting}>
            {starting ? t('linked.linking') : t('linked.link', {provider})}
          </Button>
        )}
      </div>
    </div>
  )
}
