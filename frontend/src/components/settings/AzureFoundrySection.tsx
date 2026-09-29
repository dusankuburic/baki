import {useState, useCallback, useEffect, useId, useRef} from 'react'
import {useTranslation} from 'react-i18next'
import Input from '@/components/shared/Input'
import Button from '@/components/shared/Button'
import SegmentedControl from '@/components/shared/SegmentedControl'
import Skeleton from '@/components/shared/Skeleton'
import {useConfirm} from '@/components/shared'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import type {
  AzureFoundryAuthInput,
  AzureFoundryAuthMode,
  AzureFoundryConfigInput,
  AzureFoundryPublicConfig,
  ProviderTestResult,
} from '@/types'
import {formatCount, formatDateTime} from '@/lib/format'
import {logger} from '@/lib/logger'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'

// Azure AI Foundry is configured with a document (endpoint, credentials and
// optional deployments), not a single key, so it gets its own section instead
// of an ApiKeyInput. The GET view never carries the secret: editing prefills
// the public fields and an empty secret keeps the stored one.

const PROVIDER_ID = 'azure-foundry'

// Host suffixes the server's validator accepts for a resource endpoint
// (azureFoundryHostSuffixes in internal/ai/azure_foundry_config.go). The
// client check only catches obvious mistakes early — the server stays
// authoritative, and its HOST_NOT_ALLOWED error still names the accepted
// hosts for anything this list might miss.
const AZURE_HOST_SUFFIXES = [
  '.openai.azure.com',
  '.services.ai.azure.com',
  '.cognitiveservices.azure.com',
  '.models.ai.azure.com',
]

// Same GUID shape the server requires for the Entra client ID (and accepts
// for the tenant ID).
const GUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// A tenant may also be given by one of its domain names
// (contoso.onmicrosoft.com): at least two DNS labels, the server's
// azureTenantDomain rule. The multi-tenant pseudo-tenants are single labels,
// so they never match.
const TENANT_DOMAIN_RE = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/i

/**
 * Whose credentials the section edits. The default is the signed-in user's
 * own document; an `org` scope goes through the org-level routes (cloud
 * only) and, without `canEdit`, renders the read-only summary — members see
 * what their organisation shares, admins manage it. A member's own
 * credentials always win over the org's; the org's are the fallback.
 */
export type AzureFoundryScope = {kind: 'user'} | {kind: 'org'; orgId: string; canEdit: boolean}

const USER_SCOPE: AzureFoundryScope = {kind: 'user'}

interface Props {
  onConfigured?: () => void
  scope?: AzureFoundryScope
  /**
   * Org scope: the deployment has no org keystore (404
   * ORG_KEYSTORE_UNAVAILABLE), so the section renders nothing and the host
   * should hide its heading too.
   */
  onUnavailable?: () => void
}

// Why the view could not be loaded. `unreadable`: the org's stored credential
// exists but cannot be decrypted or read (ORG_CREDENTIAL_UNREADABLE) — never
// shown as "not configured". `forbidden`: the caller's org role may not see
// it (403) — a note, not a retry that can only fail again.
type LoadErrorKind = 'failed' | 'unreadable' | 'forbidden'

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
  // Additional deployments as typed: comma-separated names.
  deployments: string
  // Claude deployments as typed (Anthropic Messages route): comma-separated.
  claudeDeployments: string
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
  deployments: '',
  claudeDeployments: '',
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
    deployments: (cfg.deployments ?? []).join(', '),
    claudeDeployments: (cfg.claudeDeployments ?? []).join(', '),
  }
}

// True when any field differs from its baseline. Used by cancel: discarding
// eight fields with one click deserves a confirm only when something would
// actually be lost.
function formsDiffer(a: FormState, b: FormState): boolean {
  return (Object.keys(EMPTY_FORM) as (keyof FormState)[]).some(k => a[k] !== b[k])
}

// The additional deployments field as a list: comma-separated, trimmed,
// blanks dropped, deduplicated case-insensitively (Azure deployment names
// are), first spelling kept. The server normalises again and also drops the
// chat and embedding deployments.
function parseDeployments(text: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of text.split(',')) {
    const name = raw.trim()
    if (!name || seen.has(name.toLowerCase())) continue
    seen.add(name.toLowerCase())
    out.push(name)
  }
  return out
}

// Claude deployment names that are already listed as the chat, embedding or
// an additional deployment. One deployment answers on one route, so the
// server drops them from the Claude list; the client drops them before
// sending and says so, instead of letting them vanish on save.
function claudeDuplicates(form: FormState): string[] {
  const taken = new Set(
    [form.chatDeployment.trim(), form.embeddingDeployment.trim(), ...parseDeployments(form.deployments)]
      .filter(Boolean)
      .map(n => n.toLowerCase()),
  )
  return parseDeployments(form.claudeDeployments).filter(n => taken.has(n.toLowerCase()))
}

// The Claude deployments to send: the typed list minus claudeDuplicates.
function claudeTargets(form: FormState): string[] {
  const dupes = new Set(claudeDuplicates(form).map(n => n.toLowerCase()))
  return parseDeployments(form.claudeDeployments).filter(n => !dupes.has(n.toLowerCase()))
}

// An API key cannot list the resource's deployments, so a chat deployment
// must be named — unless Entra ID with a project endpoint lists them
// (mirrors the server's MISSING_DEPLOYMENT rule). A Claude deployment is a
// chat target too: it answers on the Anthropic Messages route.
function hasChatTarget(form: FormState): boolean {
  return (
    !!form.chatDeployment.trim() ||
    parseDeployments(form.deployments).length > 0 ||
    claudeTargets(form).length > 0 ||
    (form.authMode === 'entra' && !!form.projectEndpoint.trim())
  )
}

// The i18n keys the field validators below can return (typed as literals so
// they flow straight into t()).
type AzureFieldErrorKey =
  | 'providers.azureEndpointInvalid'
  | 'providers.azureEndpointHttps'
  | 'providers.azureHostRejected'
  | 'providers.azureGuidInvalid'
  | 'providers.azureTenantInvalid'

// Lightweight client-side mirrors of the server's endpoint rules: an absolute
// https:// URL whose host is an Azure AI Foundry one. Returns the i18n key of
// the problem (shown once the field has been blurred), or null.
function endpointErrorKey(value: string): AzureFieldErrorKey | null {
  const raw = value.trim()
  if (!raw) return null
  let url: URL | null = null
  try {
    url = new URL(raw)
  } catch {
    /* unparseable — handled below */
  }
  if (!url || !url.hostname) return 'providers.azureEndpointInvalid'
  if (url.protocol !== 'https:') return 'providers.azureEndpointHttps'
  const host = url.hostname.toLowerCase()
  if (!AZURE_HOST_SUFFIXES.some(s => host.endsWith(s) && host.length > s.length)) return 'providers.azureHostRejected'
  return null
}

// Entra tenant/client IDs are GUIDs on the server; blank is left to the
// existing required-field gating.
function guidErrorKey(value: string): AzureFieldErrorKey | null {
  return value.trim() && !GUID_RE.test(value.trim()) ? 'providers.azureGuidInvalid' : null
}

// The tenant ID is a GUID or a domain name of the tenant (server rule).
function tenantErrorKey(value: string): AzureFieldErrorKey | null {
  const v = value.trim()
  if (!v || GUID_RE.test(v)) return null
  const isIPv4 = /^\d{1,3}(\.\d{1,3}){3}$/.test(v)
  return v.length <= 253 && TENANT_DOMAIN_RE.test(v) && !isIPv4 ? null : 'providers.azureTenantInvalid'
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
  const deployments = parseDeployments(form.deployments)
  if (deployments.length > 0) body.deployments = deployments
  const claudeDeployments = claudeTargets(form)
  if (claudeDeployments.length > 0) body.claudeDeployments = claudeDeployments
  return body
}

export default function AzureFoundrySection({onConfigured, scope = USER_SCOPE, onUnavailable}: Props) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  // The org routes take over for an org scope; everything else (form,
  // validation, error mapping) is shared with the user's own document.
  const orgId = scope.kind === 'org' ? scope.orgId : null
  const canEdit = scope.kind === 'org' ? scope.canEdit : true
  // null while the first GET is in flight.
  const [config, setConfig] = useState<AzureFoundryPublicConfig | null>(null)
  const [loadError, setLoadError] = useState<LoadErrorKind | null>(null)
  const loadFailed = loadError !== null
  // The org keystore does not exist in this deployment: render nothing.
  const [unavailable, setUnavailable] = useState(false)
  // A save or remove succeeded but the reload of the view failed: say so
  // (with a retry) instead of showing the pre-change view — or, on a first
  // configure, the empty form that invites saving again.
  const [changedButStale, setChangedButStale] = useState(false)
  // Org scope: another admin saved between our read and our save (409).
  const [conflict, setConflict] = useState(false)
  // Read through a ref so a host's inline callback cannot change `load`'s
  // identity and re-run the load effect on every render.
  const onUnavailableRef = useRef(onUnavailable)
  useEffect(() => {
    onUnavailableRef.current = onUnavailable
  })
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState<FormState>(EMPTY_FORM)
  const [showSecret, setShowSecret] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ok: boolean; latencyMs?: number; error?: string} | null>(null)
  const [removing, setRemoving] = useState(false)
  const [removeError, setRemoveError] = useState<string | null>(null)
  // True while a save/remove waits on the reload of the view: the summary is
  // held as a skeleton so the stale (or, on first configure, empty) view
  // cannot flash while the no-dedup GET is in flight.
  const [refreshing, setRefreshing] = useState(false)
  // Fields the user has blurred at least once — validation errors are only
  // shown for those, so nothing turns red while typing.
  const [touched, setTouched] = useState<Set<'endpoint' | 'tenantId' | 'clientId'>>(new Set())
  // Same signal ApiKeyInput uses for "where does the secret end up".
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()

  // Resolves true when the view was (re)loaded.
  const load = useCallback(
    (): Promise<boolean> =>
      (orgId ? providersApi.getOrgAzureFoundryConfig(orgId) : providersApi.getAzureFoundryConfig())
        .then(cfg => {
          setConfig(cfg)
          setLoadError(null)
          setChangedButStale(false)
          return true
        })
        .catch(err => {
          // A failed GET must not render as "not configured": the empty
          // form invites saving over the stored credentials. A distinct
          // error row with a retry shows instead.
          logger.warn('Failed to load Azure AI Foundry config', err)
          if (orgId && err instanceof ApiError) {
            if (err.code === 'ORG_KEYSTORE_UNAVAILABLE') {
              setUnavailable(true)
              onUnavailableRef.current?.()
              return false
            }
            if (err.status === 403) {
              setLoadError('forbidden')
              return false
            }
            if (err.code === 'ORG_CREDENTIAL_UNREADABLE') {
              setLoadError('unreadable')
              return false
            }
          }
          setLoadError('failed')
          return false
        }),
    [orgId],
  )

  useEffect(() => {
    void load()
  }, [load])

  const update = (patch: Partial<FormState>) => setForm(prev => ({...prev, ...patch}))
  const touch = (field: 'endpoint' | 'tenantId' | 'clientId') =>
    setTouched(prev => (prev.has(field) ? prev : new Set(prev).add(field)))

  const configured = config?.configured === true
  const showForm = config !== null && (!configured || editing)
  // The stored secret can only be kept when it is the same kind: switching
  // from an API key to Entra ID (or back) needs a fresh secret.
  const secretStored = editing && config?.configured === true && config.hasSecret && config.authMode === form.authMode
  const secret = form.authMode === 'api_key' ? form.apiKey : form.clientSecret
  const entraIdsMissing = form.authMode === 'entra' && (!form.tenantId.trim() || !form.clientId.trim())
  const chatTargetMissing = !hasChatTarget(form)
  const claudeDupes = claudeDuplicates(form)
  const endpointErr: AzureFieldErrorKey | null = endpointErrorKey(form.endpoint)
  const tenantErr: AzureFieldErrorKey | null = form.authMode === 'entra' ? tenantErrorKey(form.tenantId) : null
  const clientErr: AzureFieldErrorKey | null = form.authMode === 'entra' ? guidErrorKey(form.clientId) : null
  // The suffix hint does not block Save: operators can extend the server's
  // host allow-list (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS) past the standard
  // Azure suffixes, and the server's HOST_NOT_ALLOWED error gives the
  // precise verdict for everyone else. Only malformations the server would
  // always reject (unparseable/non-https URL, non-GUID ids) hard-block.
  const formInvalid = (!!endpointErr && endpointErr !== 'providers.azureHostRejected') || !!tenantErr || !!clientErr
  const canSave =
    !saving &&
    !!form.endpoint.trim() &&
    (secretStored || !!secret.trim()) &&
    !entraIdsMissing &&
    !chatTargetMissing &&
    !formInvalid

  const resetFormState = useCallback(() => {
    setForm(EMPTY_FORM)
    setShowSecret(false)
    setSaveError(null)
    setConflict(false)
    setTouched(new Set())
  }, [])

  const startEdit = () => {
    if (config) setForm(formFromConfig(config))
    setShowSecret(false)
    setSaveError(null)
    setTestResult(null)
    setEditing(true)
  }

  const cancelEdit = useCallback(async () => {
    if (config) {
      const baseline = config.configured ? formFromConfig(config) : EMPTY_FORM
      if (formsDiffer(form, baseline)) {
        const ok = await confirm({
          title: t('providers.azureDiscardTitle'),
          message: t('providers.azureDiscardMessage'),
          danger: true,
          confirmLabel: t('providers.azureDiscard'),
        })
        if (!ok) return
      }
    }
    setEditing(false)
    resetFormState()
  }, [confirm, config, form, resetFormState, t])

  const handleSave = useCallback(async () => {
    setSaving(true)
    setSaveError(null)
    setConflict(false)
    try {
      const body = toInput(form)
      if (orgId) {
        // Compare-and-swap: the server refuses the save (409) when another
        // admin changed the shared document since this view was read.
        const updatedAt = config?.configured ? config.updatedAt : undefined
        await providersApi.saveOrgAzureFoundryConfig(orgId, updatedAt ? {...body, updatedAt} : body)
      } else {
        await providersApi.saveAzureFoundryConfig(body)
      }
      setEditing(false)
      resetFormState()
      setTestResult(null)
      setRefreshing(true)
      const reloaded = await load()
      setRefreshing(false)
      if (!reloaded) setChangedButStale(true)
      onConfigured?.()
    } catch (err) {
      if (orgId && err instanceof ApiError && err.status === 409) {
        setConflict(true)
      } else if (orgId && err instanceof ApiError && err.status === 403) {
        // The caller's admin role went away since the panel rendered.
        setSaveError(t('providers.azureOrgReadOnly'))
      } else if (err instanceof ApiError) {
        // Validation codes carry an actionable server message. Two get local
        // copy: HOST_NOT_ALLOWED is the RESOURCE endpoint's allow-list verdict
        // (the copy names the accepted hosts) and MISSING_DEPLOYMENT explains
        // the API-key listing limit. A rejected project endpoint
        // (INVALID_PROJECT_ENDPOINT) and an unknown or misspelled field
        // (INVALID_CONFIG) keep the server's text, which names the value at
        // fault — the host copy would blame a resource endpoint that is fine.
        switch (err.code) {
          case 'HOST_NOT_ALLOWED':
            setSaveError(t('providers.azureHostRejected'))
            break
          case 'MISSING_DEPLOYMENT':
            setSaveError(t('providers.azureMissingDeployment'))
            break
          case 'INVALID_PROJECT_ENDPOINT':
          case 'INVALID_CONFIG':
          default:
            setSaveError(err.message || t('providers.azureSaveFailed'))
        }
      } else {
        logger.warn('Save Azure AI Foundry config failed:', err)
        setSaveError(t('providers.azureSaveFailed'))
      }
    } finally {
      setSaving(false)
    }
  }, [config, form, load, onConfigured, orgId, resetFormState, t])

  // The conflict notice's action: drop the edit and show the configuration
  // the other admin saved.
  const reloadAfterConflict = useCallback(async () => {
    setConflict(false)
    setEditing(false)
    resetFormState()
    setRefreshing(true)
    await load()
    setRefreshing(false)
  }, [load, resetFormState])

  const handleRemove = useCallback(async () => {
    const ok = await confirm({
      title: t('providers.azureRemoveTitle'),
      message: t('providers.azureRemoveMessage'),
      danger: true,
      confirmLabel: t('keys.remove'),
    })
    if (!ok) return
    setRemoving(true)
    setRemoveError(null)
    try {
      await (orgId ? providersApi.deleteOrgAzureFoundryConfig(orgId) : providersApi.deleteApiKey(PROVIDER_ID))
      setTestResult(null)
      setRefreshing(true)
      const reloaded = await load()
      setRefreshing(false)
      if (!reloaded) setChangedButStale(true)
      onConfigured?.()
    } catch (err) {
      logger.warn('Remove Azure AI Foundry config failed:', err)
      setRemoveError(
        orgId && err instanceof ApiError && err.status === 403
          ? t('providers.azureOrgReadOnly')
          : err instanceof ApiError
            ? err.message || t('providers.azureRemoveFailed')
            : t('providers.azureRemoveFailed'),
      )
    } finally {
      setRemoving(false)
    }
  }, [confirm, load, onConfigured, orgId, t])

  const handleTest = useCallback(async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const result: ProviderTestResult | null = await (orgId
        ? providersApi.testOrgAzureFoundryConnection(orgId)
        : providersApi.testProviderConnection(PROVIDER_ID))
      // Unlike a plain key check, Azure failures are actionable (wrong
      // deployment name, missing role) — keep the server's text. A null body
      // carries no reason, so it gets the generic failure copy instead of a
      // bare "Invalid".
      if (result?.ok) {
        setTestResult({ok: true, latencyMs: result.latencyMs})
      } else {
        setTestResult({ok: false, error: result?.error || t('providers.azureTestFailed')})
      }
    } catch (err) {
      setTestResult({
        ok: false,
        error:
          orgId && err instanceof ApiError && err.status === 403
            ? t('providers.azureOrgReadOnly')
            : err instanceof Error
              ? err.message
              : t('providers.azureTestFailed'),
      })
    } finally {
      setTesting(false)
    }
  }, [orgId, t])

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
    deployments: `${uid}-deployments`,
    claudeDeployments: `${uid}-claude-deployments`,
  }

  if (unavailable) return null

  if (config === null && !loadFailed) {
    return <Skeleton lines={2} className="max-w-md" />
  }

  // The caller's org role may not see the shared configuration: a note — a
  // retry could only fail the same way.
  if (loadError === 'forbidden') {
    return <p className="text-sm text-text-secondary">{t('providers.azureOrgNoAccess')}</p>
  }

  // A failed load is never "not configured": an error row with a retry, and
  // (when some earlier view exists) that view underneath it — never the
  // empty form that saving would overwrite the stored credentials from. An
  // unreadable org credential says so, and lets an admin replace it.
  const loadErrorRow = (
    <div role="alert" className="flex items-center gap-2 flex-wrap">
      <span className="text-xs text-red-400">
        {loadError === 'unreadable' ? t('providers.azureOrgUnreadable') : t('providers.azureLoadFailed')}
      </span>
      <Button size="sm" variant="secondary" onClick={() => void load()}>
        {t('providers.azureRetry')}
      </Button>
      {loadError === 'unreadable' && canEdit && (
        <Button
          size="sm"
          variant="secondary"
          onClick={() => {
            // Replacing is the fix: an empty form whose save overwrites the
            // unreadable document.
            setLoadError(null)
            setConfig({configured: false})
          }}
        >
          {t('providers.azureOrgReplace')}
        </Button>
      )}
    </div>
  )
  if (config === null && !refreshing) {
    return <div className="space-y-3 max-w-2xl">{loadErrorRow}</div>
  }

  // Save/remove just succeeded: hold a skeleton until the fresh view lands.
  if (refreshing || config === null) {
    return <Skeleton lines={2} className="max-w-md" />
  }

  // Saved (or removed), but the fresh view did not load: neither the old view
  // nor an empty form may stand in for it.
  if (changedButStale) {
    return (
      <div role="status" className="flex items-center gap-2 flex-wrap max-w-2xl">
        <span className="text-xs text-text-secondary">{t('providers.azureSavedRefreshFailed')}</span>
        <Button size="sm" variant="secondary" onClick={() => void load()}>
          {t('providers.azureRetry')}
        </Button>
      </div>
    )
  }

  // A member of the org (no edit rights) never sees the form: a one-line
  // "not configured" note stands in for it.
  if (!canEdit && !config.configured) {
    return (
      <div className="space-y-3">
        {loadFailed && loadErrorRow}
        <p className="text-sm text-text-secondary">{t('providers.azureOrgNotConfigured')}</p>
      </div>
    )
  }

  if (!showForm && config.configured) {
    const authLabel = config.authMode === 'entra' ? t('providers.azureAuthEntra') : t('providers.azureAuthApiKey')
    // The "configured" line is auth-mode aware: an Entra setup stores a
    // client secret, not an API key.
    const configuredLine =
      config.authMode === 'entra'
        ? localBackend
          ? t('keys.entraConfiguredLocal')
          : t('keys.entraConfiguredServer')
        : localBackend
          ? t('keys.configuredLocal')
          : t('keys.configuredServer')
    return (
      <div className="space-y-3">
        {loadFailed && loadErrorRow}
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
          {config.deployments && config.deployments.length > 0 && (
            <>
              <dt className="text-text-tertiary">{t('providers.azureConfiguredDeployments')}</dt>
              <dd className="break-all">
                <code className="font-mono text-xs text-text-primary">{config.deployments.join(', ')}</code>
              </dd>
            </>
          )}
          {config.claudeDeployments && config.claudeDeployments.length > 0 && (
            <>
              <dt className="text-text-tertiary">{t('providers.azureConfiguredClaudeDeployments')}</dt>
              <dd className="break-all">
                <code className="font-mono text-xs text-text-primary">{config.claudeDeployments.join(', ')}</code>
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
        {scope.kind === 'org' && (
          <p className="text-xs text-text-tertiary">
            <span>
              {config.updatedBy
                ? t('providers.azureOrgSharedBy', {name: config.updatedBy})
                : t('providers.azureOrgShared')}
            </span>
            {config.updatedAt && (
              <>
                {' · '}
                <time dateTime={config.updatedAt}>
                  {t('providers.azureOrgUpdatedAt', {date: formatDateTime(config.updatedAt)})}
                </time>
              </>
            )}
          </p>
        )}
        {!canEdit && <p className="text-xs text-text-tertiary">{t('providers.azureOrgReadOnly')}</p>}
        {canEdit && (
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-sm text-text-secondary">{configuredLine}</span>
            <Button size="sm" variant="secondary" onClick={startEdit} disabled={removing}>
              {t('providers.azureEdit')}
            </Button>
            <Button size="sm" variant="secondary" onClick={handleRemove} disabled={removing}>
              {removing ? t('keys.removing') : t('keys.remove')}
            </Button>
            <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing || removing}>
              {testing ? t('keys.testing') : t('keys.test')}
            </Button>
            {/* Announced to assistive technology. The regions are real boxes
              (display:contents drops the element — and with it the role — in
              some browser/screen-reader pairs); the status region stays
              mounted so a pass is read out politely, and "Invalid" plus the
              server's reason form one alert. */}
            <span role="status" className="inline-flex items-center text-xs text-green-400">
              {testResult?.ok &&
                (typeof testResult.latencyMs === 'number' && Number.isFinite(testResult.latencyMs)
                  ? t('apiKey.validLatency', {ms: formatCount(testResult.latencyMs)})
                  : t('apiKey.valid'))}
            </span>
            {testResult && !testResult.ok && (
              <span role="alert" className="inline-flex items-center gap-2 flex-wrap text-xs text-red-400">
                <span>{t('apiKey.invalid')}</span>
                {testResult.error && <span>{testResult.error}</span>}
              </span>
            )}
            {removeError && (
              <span role="alert" className="text-xs text-red-400">
                {removeError}
              </span>
            )}
          </div>
        )}
      </div>
    )
  }

  const secretPlaceholder = secretStored ? t('providers.azureSecretUnchanged') : undefined
  // Shared by whichever secret field is on screen (only one at a time).
  const secretInputProps = {
    type: showSecret ? 'text' : 'password',
    // A secret: no autofill/suggestions ("new-password" — password managers
    // ignore "off" on password fields and would fill the user's saved site
    // password), and no spellcheck — once shown as plain text, browser
    // spellcheck services could receive it.
    autoComplete: 'new-password',
    spellCheck: false,
    autoCapitalize: 'off',
    autoCorrect: 'off',
    className: 'flex-1',
  } as const

  return (
    <div className="space-y-3 max-w-2xl">
      {loadFailed && loadErrorRow}
      <Field id={ids.endpoint} label={t('providers.azureEndpoint')}>
        <Input
          id={ids.endpoint}
          value={form.endpoint}
          onChange={e => update({endpoint: e.target.value})}
          onBlur={() => touch('endpoint')}
          placeholder={t('providers.azureEndpointPlaceholder')}
          aria-label={t('providers.azureEndpointAria')}
          hint={t('providers.azureEndpointHint')}
          error={touched.has('endpoint') && endpointErr ? t(endpointErr) : undefined}
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
            <SecretToggle shown={showSecret} field={t('providers.azureApiKey')} onToggle={setShowSecret} />
          </div>
        </Field>
      ) : (
        <>
          <Field id={ids.tenantId} label={t('providers.azureTenantId')}>
            <Input
              id={ids.tenantId}
              value={form.tenantId}
              onChange={e => update({tenantId: e.target.value})}
              onBlur={() => touch('tenantId')}
              error={touched.has('tenantId') && tenantErr ? t(tenantErr) : undefined}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field id={ids.clientId} label={t('providers.azureClientId')}>
            <Input
              id={ids.clientId}
              value={form.clientId}
              onChange={e => update({clientId: e.target.value})}
              onBlur={() => touch('clientId')}
              error={touched.has('clientId') && clientErr ? t(clientErr) : undefined}
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
              <SecretToggle shown={showSecret} field={t('providers.azureClientSecret')} onToggle={setShowSecret} />
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

      <Field id={ids.deployments} label={t('providers.azureDeployments')}>
        <Input
          id={ids.deployments}
          value={form.deployments}
          onChange={e => update({deployments: e.target.value})}
          placeholder={t('providers.azureDeploymentsPlaceholder')}
          hint={t('providers.azureDeploymentsHint')}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>

      <Field id={ids.claudeDeployments} label={t('providers.azureClaudeDeployments')}>
        <Input
          id={ids.claudeDeployments}
          value={form.claudeDeployments}
          onChange={e => update({claudeDeployments: e.target.value})}
          placeholder={t('providers.azureClaudeDeploymentsPlaceholder')}
          hint={t('providers.azureClaudeDeploymentsHint')}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>

      {claudeDupes.length > 0 && (
        <p className="text-xs text-text-secondary">
          {t('providers.azureClaudeDuplicates', {count: claudeDupes.length, names: claudeDupes.join(', ')})}
        </p>
      )}

      {form.endpoint.trim() && chatTargetMissing && (
        <p className="text-xs text-text-secondary">{t('providers.azureMissingDeployment')}</p>
      )}

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
        {saveError && (
          <span role="alert" className="text-xs text-red-400">
            {saveError}
          </span>
        )}
      </div>
      {conflict && (
        <div role="alert" className="flex items-center gap-2 flex-wrap">
          <span className="text-xs text-red-400">{t('providers.azureOrgConflict')}</span>
          <Button size="sm" variant="secondary" onClick={() => void reloadAfterConflict()}>
            {t('providers.azureOrgReload')}
          </Button>
        </div>
      )}
    </div>
  )
}

// Show/Hide for a secret field: a toggle button (aria-pressed) whose name
// includes the field it reveals — two bare "Show" buttons were
// indistinguishable in a screen reader's button list.
function SecretToggle({shown, field, onToggle}: {shown: boolean; field: string; onToggle: (next: boolean) => void}) {
  const {t} = useTranslation('settings')
  return (
    <Button
      size="sm"
      variant="secondary"
      onClick={() => onToggle(!shown)}
      aria-pressed={shown}
      aria-label={shown ? t('providers.azureHideSecretAria', {field}) : t('providers.azureShowSecretAria', {field})}
    >
      {shown ? t('apiKey.hide') : t('apiKey.show')}
    </Button>
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
