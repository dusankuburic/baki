import {useState, useCallback, useEffect, useId} from 'react'
import {useTranslation} from 'react-i18next'
import Input from '@/components/shared/Input'
import Button from '@/components/shared/Button'
import SegmentedControl from '@/components/shared/SegmentedControl'
import Skeleton from '@/components/shared/Skeleton'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import type {
  AzureFoundryAuthInput,
  AzureFoundryAuthMode,
  AzureFoundryConfigInput,
  AzureFoundryPublicConfig,
  ProviderTestResult,
} from '@/types'
import {logger} from '@/lib/logger'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'

// Azure AI Foundry is configured with a document (endpoint, credentials and
// optional deployments), not a single key, so it gets its own section instead
// of an ApiKeyInput. The GET view never carries the secret: editing prefills
// the public fields and an empty secret keeps the stored one.

const PROVIDER_ID = 'azure-foundry'

interface Props {
  onConfigured?: () => void
}

interface FormState {
  endpoint: string
  authMode: AzureFoundryAuthMode
  apiKey: string
  tenantId: string
  clientId: string
  clientSecret: string
  projectEndpoint: string
  chatDeployment: string
  embeddingDeployment: string
}

const EMPTY_FORM: FormState = {
  endpoint: '',
  authMode: 'api_key',
  apiKey: '',
  tenantId: '',
  clientId: '',
  clientSecret: '',
  projectEndpoint: '',
  chatDeployment: '',
  embeddingDeployment: '',
}

function formFromConfig(cfg: AzureFoundryPublicConfig): FormState {
  if (!cfg.configured) return EMPTY_FORM
  return {
    ...EMPTY_FORM,
    endpoint: cfg.endpoint,
    authMode: cfg.authMode,
    tenantId: cfg.tenantId ?? '',
    clientId: cfg.clientId ?? '',
    projectEndpoint: cfg.projectEndpoint ?? '',
    chatDeployment: cfg.chatDeployment ?? '',
    embeddingDeployment: cfg.embeddingDeployment ?? '',
  }
}

// The POST body: trimmed, blank optional fields dropped, and a blank secret
// omitted entirely (never sent as "") so the server keeps the stored one.
function toInput(form: FormState): AzureFoundryConfigInput {
  const auth: AzureFoundryAuthInput =
    form.authMode === 'api_key'
      ? {mode: 'api_key', ...(form.apiKey.trim() ? {key: form.apiKey.trim()} : {})}
      : {
          mode: 'entra',
          tenantId: form.tenantId.trim(),
          clientId: form.clientId.trim(),
          ...(form.clientSecret.trim() ? {clientSecret: form.clientSecret.trim()} : {}),
        }
  const body: AzureFoundryConfigInput = {endpoint: form.endpoint.trim(), auth}
  if (form.projectEndpoint.trim()) body.projectEndpoint = form.projectEndpoint.trim()
  if (form.chatDeployment.trim()) body.chatDeployment = form.chatDeployment.trim()
  if (form.embeddingDeployment.trim()) body.embeddingDeployment = form.embeddingDeployment.trim()
  return body
}

export default function AzureFoundrySection({onConfigured}: Props) {
  const {t} = useTranslation('settings')
  // null while the first GET is in flight.
  const [config, setConfig] = useState<AzureFoundryPublicConfig | null>(null)
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState<FormState>(EMPTY_FORM)
  const [showSecret, setShowSecret] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ok: boolean; error?: string} | null>(null)
  // Same signal ApiKeyInput uses for "where does the secret end up".
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()

  const load = useCallback(
    () =>
      providersApi
        .getAzureFoundryConfig()
        .then(cfg => setConfig(cfg))
        .catch(err => {
          // A backend without the route (or a transient failure) shows the
          // empty form rather than blocking the whole accounts page.
          logger.warn('Failed to load Azure AI Foundry config', err)
          setConfig({configured: false})
        }),
    [],
  )

  useEffect(() => {
    void load()
  }, [load])

  const update = (patch: Partial<FormState>) => setForm(prev => ({...prev, ...patch}))

  const configured = config?.configured === true
  const showForm = config !== null && (!configured || editing)
  // The stored secret can only be kept when it is the same kind: switching
  // from an API key to Entra ID (or back) needs a fresh secret.
  const secretStored = editing && config?.configured === true && config.hasSecret && config.authMode === form.authMode
  const secret = form.authMode === 'api_key' ? form.apiKey : form.clientSecret
  const entraIdsMissing = form.authMode === 'entra' && (!form.tenantId.trim() || !form.clientId.trim())
  const canSave = !saving && !!form.endpoint.trim() && (secretStored || !!secret.trim()) && !entraIdsMissing

  const startEdit = () => {
    if (config) setForm(formFromConfig(config))
    setShowSecret(false)
    setSaveError(null)
    setTestResult(null)
    setEditing(true)
  }

  const cancelEdit = () => {
    setEditing(false)
    setForm(EMPTY_FORM)
    setShowSecret(false)
    setSaveError(null)
  }

  const handleSave = useCallback(async () => {
    setSaving(true)
    setSaveError(null)
    try {
      await providersApi.saveAzureFoundryConfig(toInput(form))
      setForm(EMPTY_FORM)
      setShowSecret(false)
      setEditing(false)
      setTestResult(null)
      await load()
      onConfigured?.()
    } catch (err) {
      if (err instanceof ApiError) {
        // Validation codes carry an actionable server message; the host
        // allow-list one gets local copy that names the accepted hosts.
        setSaveError(
          err.code === 'HOST_NOT_ALLOWED'
            ? t('providers.azureHostRejected')
            : err.message || t('providers.azureSaveFailed'),
        )
      } else {
        logger.warn('Save Azure AI Foundry config failed:', err)
        setSaveError(t('providers.azureSaveFailed'))
      }
    } finally {
      setSaving(false)
    }
  }, [form, load, onConfigured, t])

  const handleRemove = useCallback(async () => {
    try {
      await providersApi.deleteApiKey(PROVIDER_ID)
      setTestResult(null)
      await load()
      onConfigured?.()
    } catch (err) {
      logger.warn('Remove Azure AI Foundry config failed:', err)
    }
  }, [load, onConfigured])

  const handleTest = useCallback(async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const result: ProviderTestResult | null = await providersApi.testProviderConnection(PROVIDER_ID)
      // Unlike a plain key check, Azure failures are actionable (wrong
      // deployment name, missing role) — keep the server's text.
      setTestResult(result?.ok ? {ok: true} : {ok: false, error: result?.error})
    } catch (err) {
      setTestResult({ok: false, error: err instanceof Error ? err.message : undefined})
    } finally {
      setTesting(false)
    }
  }, [])

  const uid = useId()
  const ids = {
    endpoint: `${uid}-endpoint`,
    apiKey: `${uid}-api-key`,
    tenantId: `${uid}-tenant`,
    clientId: `${uid}-client`,
    clientSecret: `${uid}-client-secret`,
    projectEndpoint: `${uid}-project`,
    chatDeployment: `${uid}-chat`,
    embeddingDeployment: `${uid}-embedding`,
  }

  if (config === null) {
    return <Skeleton lines={2} className="max-w-md" />
  }

  if (!showForm && config.configured) {
    const authLabel = config.authMode === 'entra' ? t('providers.azureAuthEntra') : t('providers.azureAuthApiKey')
    return (
      <div className="space-y-3">
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
          <dt className="text-text-tertiary">{t('providers.azureConfiguredEndpoint')}</dt>
          <dd>
            <code className="font-mono text-xs text-text-primary break-all">{config.endpoint}</code>
          </dd>
          <dt className="text-text-tertiary">{t('providers.azureConfiguredAuth')}</dt>
          <dd className="text-text-primary">{authLabel}</dd>
          {config.projectEndpoint && (
            <>
              <dt className="text-text-tertiary">{t('providers.azureConfiguredProject')}</dt>
              <dd>
                <code className="font-mono text-xs text-text-primary break-all">{config.projectEndpoint}</code>
              </dd>
            </>
          )}
          {config.chatDeployment && (
            <>
              <dt className="text-text-tertiary">{t('providers.azureConfiguredChatDeployment')}</dt>
              <dd>
                <code className="font-mono text-xs text-text-primary">{config.chatDeployment}</code>
              </dd>
            </>
          )}
          {config.embeddingDeployment && (
            <>
              <dt className="text-text-tertiary">{t('providers.azureConfiguredEmbeddingDeployment')}</dt>
              <dd>
                <code className="font-mono text-xs text-text-primary">{config.embeddingDeployment}</code>
              </dd>
            </>
          )}
        </dl>
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-sm text-text-secondary">
            {localBackend ? t('keys.configuredLocal') : t('keys.configuredServer')}
          </span>
          <Button size="sm" variant="secondary" onClick={startEdit}>
            {t('providers.azureEdit')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleRemove}>
            {t('keys.remove')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing}>
            {testing ? t('keys.testing') : t('keys.test')}
          </Button>
          {testResult?.ok && <span className="text-xs text-green-400">{t('apiKey.valid')}</span>}
          {testResult && !testResult.ok && (
            <>
              <span className="text-xs text-red-400">{t('apiKey.invalid')}</span>
              {testResult.error && <span className="text-xs text-red-400">{testResult.error}</span>}
            </>
          )}
        </div>
      </div>
    )
  }

  const secretPlaceholder = secretStored ? t('providers.azureSecretUnchanged') : undefined
  // Shared by whichever secret field is on screen (only one at a time).
  const secretInputProps = {
    type: showSecret ? 'text' : 'password',
    // A secret: no autofill/suggestions, and no spellcheck — once shown as
    // plain text, browser spellcheck services could receive it.
    autoComplete: 'off',
    spellCheck: false,
    autoCapitalize: 'off',
    autoCorrect: 'off',
    className: 'flex-1',
  } as const

  return (
    <div className="space-y-3 max-w-2xl">
      <Field id={ids.endpoint} label={t('providers.azureEndpoint')}>
        <Input
          id={ids.endpoint}
          value={form.endpoint}
          onChange={e => update({endpoint: e.target.value})}
          placeholder={t('providers.azureEndpointPlaceholder')}
          aria-label={t('providers.azureEndpointAria')}
          hint={t('providers.azureEndpointHint')}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>

      <div className="space-y-1">
        <span className="block text-xs font-medium text-text-secondary">{t('providers.azureAuthMode')}</span>
        <SegmentedControl
          size="sm"
          value={form.authMode}
          onChange={mode => {
            setShowSecret(false)
            update({authMode: mode})
          }}
          options={[
            {value: 'api_key', label: t('providers.azureAuthApiKey')},
            {value: 'entra', label: t('providers.azureAuthEntra')},
          ]}
          label={t('providers.azureAuthMode')}
        />
      </div>

      {form.authMode === 'api_key' ? (
        <Field id={ids.apiKey} label={t('providers.azureApiKey')}>
          <div className="flex items-center gap-2">
            <Input
              {...secretInputProps}
              id={ids.apiKey}
              value={form.apiKey}
              onChange={e => update({apiKey: e.target.value})}
              placeholder={secretPlaceholder ?? t('apiKey.placeholder')}
              aria-label={t('providers.azureApiKeyAria')}
            />
            <Button size="sm" variant="secondary" onClick={() => setShowSecret(!showSecret)}>
              {showSecret ? t('apiKey.hide') : t('apiKey.show')}
            </Button>
          </div>
        </Field>
      ) : (
        <>
          <Field id={ids.tenantId} label={t('providers.azureTenantId')}>
            <Input
              id={ids.tenantId}
              value={form.tenantId}
              onChange={e => update({tenantId: e.target.value})}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field id={ids.clientId} label={t('providers.azureClientId')}>
            <Input
              id={ids.clientId}
              value={form.clientId}
              onChange={e => update({clientId: e.target.value})}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field id={ids.clientSecret} label={t('providers.azureClientSecret')}>
            <div className="flex items-center gap-2">
              <Input
                {...secretInputProps}
                id={ids.clientSecret}
                value={form.clientSecret}
                onChange={e => update({clientSecret: e.target.value})}
                placeholder={secretPlaceholder}
                aria-label={t('providers.azureClientSecretAria')}
              />
              <Button size="sm" variant="secondary" onClick={() => setShowSecret(!showSecret)}>
                {showSecret ? t('apiKey.hide') : t('apiKey.show')}
              </Button>
            </div>
          </Field>
        </>
      )}

      <Field id={ids.projectEndpoint} label={t('providers.azureProjectEndpoint')}>
        <Input
          id={ids.projectEndpoint}
          value={form.projectEndpoint}
          onChange={e => update({projectEndpoint: e.target.value})}
          placeholder={t('providers.azureProjectEndpointPlaceholder')}
          hint={t('providers.azureProjectEndpointHint')}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>

      <div className="grid gap-3 sm:grid-cols-2">
        <Field id={ids.chatDeployment} label={t('providers.azureChatDeployment')}>
          <Input
            id={ids.chatDeployment}
            value={form.chatDeployment}
            onChange={e => update({chatDeployment: e.target.value})}
            placeholder={t('providers.azureDeploymentPlaceholder')}
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        <Field id={ids.embeddingDeployment} label={t('providers.azureEmbeddingDeployment')}>
          <Input
            id={ids.embeddingDeployment}
            value={form.embeddingDeployment}
            onChange={e => update({embeddingDeployment: e.target.value})}
            placeholder={t('providers.azureEmbeddingDeploymentPlaceholder')}
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
      </div>

      <p className="text-xs text-text-tertiary">{t('providers.azureAgentsHint')}</p>

      <div className="flex items-center gap-2 flex-wrap">
        <Button size="sm" variant="primary" onClick={handleSave} disabled={!canSave}>
          {saving ? t('providers.azureSaving') : t('providers.azureSave')}
        </Button>
        {editing && (
          <Button size="sm" variant="secondary" onClick={cancelEdit} disabled={saving}>
            {t('providers.azureCancel')}
          </Button>
        )}
        {saveError && <span className="text-xs text-red-400">{saveError}</span>}
      </div>
    </div>
  )
}

function Field({id, label, children}: {id: string; label: string; children: React.ReactNode}) {
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-xs font-medium text-text-secondary">
        {label}
      </label>
      {children}
    </div>
  )
}
