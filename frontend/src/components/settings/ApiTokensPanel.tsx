import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {KeyRound, Plus, Trash2, Copy, Check, AlertCircle, AlertTriangle} from 'lucide-react'
import {authApi, API_TOKEN_SCOPES, type ApiToken, type ApiTokenScope, type CreatedApiToken} from '@/api/auth'
import {writeClipboard} from '@/lib/clipboard'
import {Button, Input, Spinner, useToast, useConfirm} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'

// Least-privilege default for a CI/automation token: it can read reports and
// save flows / triage, but cannot spend AI budget (chat) or touch admin
// routes. Unchecking everything mints an unscoped token (full access).
const DEFAULT_SCOPES: ApiTokenScope[] = ['read', 'write']

/**
 * Cloud-only Settings panel for machine API tokens (personal access tokens):
 * create them for CI/automation, see existing ones, and revoke. The raw secret
 * is shown exactly once, right after creation. Gated by `!isTauri()` upstream in
 * SettingsModal.
 */
export default function ApiTokensPanel() {
  const {t} = useTranslation('settings')
  const [name, setName] = useState('')
  const [expiresDays, setExpiresDays] = useState('')
  const [scopes, setScopes] = useState<ApiTokenScope[]>(DEFAULT_SCOPES)
  const [expiryError, setExpiryError] = useState<string | null>(null)

  const toggleScope = (scope: ApiTokenScope) =>
    setScopes(prev => (prev.includes(scope) ? prev.filter(s => s !== scope) : [...prev, scope]))

  const scopeLabel = (tok: ApiToken) =>
    tok.scopes && tok.scopes.length > 0
      ? tok.scopes.map(s => t(`tokens.scope.${s}`)).join(', ')
      : t('tokens.fullAccess')
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedApiToken | null>(null) // one-time secret reveal
  const [copied, setCopied] = useState(false)

  const toast = useToast()
  const {confirm} = useConfirm()

  const {
    data,
    isLoading: loading,
    error: loadError,
    refetch: load,
  } = useAsync<ApiToken[]>(() => authApi.listApiTokens().then(t => t ?? []), [])
  const tokens = data ?? []

  const handleCreate = async () => {
    const n = name.trim()
    if (!n) return
    // Explicit expiry validation (U4.3): a non-numeric or negative value
    // used to be silently coerced into "never expires".
    if (expiresDays.trim() !== '') {
      const days = Number(expiresDays)
      if (!Number.isInteger(days) || days <= 0) {
        setExpiryError('Expiry must be a whole number of days (or empty for no expiry).')
        return
      }
    }
    setExpiryError(null)
    setCreating(true)
    try {
      const days = parseInt(expiresDays, 10)
      const tok = await authApi.createApiToken(n, Number.isFinite(days) && days > 0 ? days : undefined, scopes)
      setCreated(tok)
      setCopied(false)
      setName('')
      setExpiresDays('')
      setScopes(DEFAULT_SCOPES)
      load()
    } catch (e) {
      toast.error(t('tokens.createFailed', {message: e instanceof Error ? e.message : String(e)}))
    } finally {
      setCreating(false)
    }
  }

  const handleCopy = async () => {
    if (!created) return
    try {
      await writeClipboard(created.token)
      setCopied(true)
      toast.success(t('tokens.copied'))
    } catch {
      toast.error(t('tokens.copyFailed'))
    }
  }

  const handleRevoke = async (tok: ApiToken) => {
    const ok = await confirm({
      title: t('tokens.revokeTitle'),
      message: t('tokens.revokeMessage', {name: tok.name || tok.id}),
      danger: true,
      confirmLabel: t('tokens.revoke'),
    })
    if (!ok) return
    try {
      await authApi.revokeApiToken(tok.id)
      if (created?.id === tok.id) setCreated(null)
      toast.success(t('tokens.revoked'))
      load()
    } catch (e) {
      toast.error(t('tokens.revokeFailed', {message: e instanceof Error ? e.message : String(e)}))
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-xl font-semibold text-text-primary flex items-center gap-2">
          <KeyRound size={20} className="text-brand-500" />
          {t('tokens.title')}
        </h2>
        <p className="text-sm text-text-secondary mt-1">
          {t('tokens.subtitlePrefix')} <code className="text-brand-400">{'Authorization: Bearer pad_pat_…'}</code>
          {t('tokens.subtitleSuffix')}
        </p>
      </div>

      {/* One-time secret reveal */}
      {created && (
        <div className="p-4 border border-semantic-warning/40 bg-semantic-warning/5 rounded-xl space-y-3 animate-fade-in">
          <div className="flex items-start gap-2">
            <AlertTriangle size={18} className="text-semantic-warning shrink-0 mt-0.5" />
            <p className="text-sm text-text-primary">{t('tokens.revealWarning')}</p>
          </div>
          <div className="flex items-center gap-2">
            <code className="flex-1 px-3 py-2 rounded-lg bg-surface-2 text-xs text-text-primary break-all select-all">
              {created.token}
            </code>
            <Button size="sm" variant="secondary" onClick={handleCopy}>
              {copied ? <Check size={14} /> : <Copy size={14} />}
              {copied ? t('tokens.copiedShort') : t('tokens.copy')}
            </Button>
          </div>
        </div>
      )}

      {/* Create form */}
      <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-3">
        <h3 className="text-sm font-bold text-text-primary">{t('tokens.createTitle')}</h3>
        <div className="flex flex-col sm:flex-row gap-2">
          <Input
            className="flex-1"
            placeholder={t('tokens.namePlaceholder')}
            aria-label={t('tokens.nameAria')}
            value={name}
            onChange={e => setName(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') void handleCreate()
            }}
          />
          <Input
            className="sm:w-40"
            type="number"
            min={0}
            placeholder={t('tokens.expiresPlaceholder')}
            aria-label={t('tokens.expiresAria')}
            value={expiresDays}
            onChange={e => {
              setExpiresDays(e.target.value)
              if (expiryError) setExpiryError(null)
            }}
            aria-invalid={!!expiryError}
          />
          <Button variant="primary" onClick={handleCreate} loading={creating} disabled={!name.trim()}>
            <Plus size={16} />
            {t('tokens.create')}
          </Button>
        </div>
        {expiryError && (
          <p className="mt-1 text-2xs text-semantic-error" role="alert">
            {expiryError}
          </p>
        )}
        <fieldset className="space-y-1.5">
          <legend className="text-xs font-medium text-text-secondary">{t('tokens.scopesLabel')}</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1.5">
            {API_TOKEN_SCOPES.map(scope => (
              <label
                key={scope}
                className="flex items-center gap-1.5 text-xs text-text-secondary cursor-pointer select-none"
              >
                <input
                  type="checkbox"
                  className="accent-brand-500 w-3.5 h-3.5"
                  checked={scopes.includes(scope)}
                  onChange={() => toggleScope(scope)}
                  aria-label={t(`tokens.scope.${scope}`)}
                />
                <span>{t(`tokens.scope.${scope}`)}</span>
                <span className="text-text-tertiary">— {t(`tokens.scopeHelp.${scope}`)}</span>
              </label>
            ))}
          </div>
          <p className="text-2xs text-text-tertiary">
            {scopes.length === 0 ? t('tokens.scopesNoneHint') : t('tokens.scopesHint')}
          </p>
        </fieldset>
      </div>

      {/* Existing tokens */}
      {loading ? (
        <div className="p-8 flex justify-center">
          <Spinner />
        </div>
      ) : loadError ? (
        <div className="p-4 flex items-start gap-3 border border-semantic-error/30 bg-semantic-error/5 rounded-xl">
          <AlertCircle className="text-semantic-error shrink-0 mt-0.5" size={18} />
          <div>
            <p className="text-sm font-medium text-semantic-error">{t('tokens.loadFailed')}</p>
            <p className="text-xs text-text-tertiary mt-1">{loadError}</p>
          </div>
        </div>
      ) : tokens.length === 0 ? (
        <p className="text-sm text-text-tertiary px-1">{t('tokens.noTokens')}</p>
      ) : (
        <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
          {tokens.map((tok, i) => (
            <div
              key={tok.id}
              className={
                i !== tokens.length - 1
                  ? 'p-4 flex items-center justify-between border-b border-border-subtle'
                  : 'p-4 flex items-center justify-between'
              }
            >
              <div className="min-w-0">
                <p className="text-sm font-medium text-text-primary truncate">{tok.name || tok.id}</p>
                <p className="text-xs text-text-tertiary mt-0.5">
                  {t('tokens.createdOn', {date: new Date(tok.createdAt).toLocaleDateString()})}
                  {' · '}
                  {tok.expiresAt
                    ? t('tokens.expiresOn', {date: new Date(tok.expiresAt).toLocaleDateString()})
                    : t('tokens.neverExpires')}
                  {' · '}
                  <span title={t('tokens.scopesLabel')}>{scopeLabel(tok)}</span>
                </p>
              </div>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => handleRevoke(tok)}
                aria-label={t('tokens.revokeAria', {name: tok.name || tok.id})}
              >
                <Trash2 size={14} className="text-semantic-error" />
                {t('tokens.revoke')}
              </Button>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
