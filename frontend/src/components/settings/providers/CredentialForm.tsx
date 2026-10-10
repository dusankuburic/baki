import {useCallback, useEffect, useId, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Button, FormField, Input, SecretField, Select, Textarea} from '@/components/shared'
import DeviceAuthLoginButton, {type DeviceAuthProvider} from '@/components/chat/DeviceAuthLoginButton'
import {providersApi} from '@/api'
import {apiPath} from '@/api/providers'
import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'
import {CREDENTIAL_FIELD_TYPES, CREDENTIAL_KINDS, isKnown} from '@/types'
import type {CredentialField, GitHubUser, ProviderSpec, ProviderTestResult} from '@/types'
import GenericProviderCard, {credentialErrorText, testResultText} from './GenericProviderCard'

export interface CredentialFormProps {
  spec: ProviderSpec
  /** A credential was saved or removed, a sign-in completed or was revoked: the host re-checks the providers. */
  onConfigured: () => void
  /** A device-flow sign-in completed: the host may make the provider the active one. */
  onSignedIn?: () => void
  /** The device flow's signed-in user, as learned on mount and after a sign-in or sign-out. */
  onUser?: (user: GitHubUser | null) => void
}

/**
 * Whether the bundle can render this entry's form: a kind it knows, field
 * types it knows, a secret to save for api_key, and the device-flow routes
 * (as paths under /api/) for oauth_device. Anything else is the generic
 * card — never a guessed input.
 */
export function formSupported(spec: ProviderSpec): boolean {
  const {kind, fields} = spec.credential
  if (!isKnown(CREDENTIAL_KINDS, kind)) return false
  if (!fields.every(f => isKnown(CREDENTIAL_FIELD_TYPES, f.type))) return false
  if (kind === 'api_key') return fields.some(f => f.type === 'secret')
  if (kind === 'oauth_device') {
    const {start, poll, revoke} = spec.endpoints
    return !!(apiPath(start) && apiPath(poll) && apiPath(revoke))
  }
  return true
}

// The section body for a catalogue entry, by its credential kind: api_key is
// the key form (POST /api/keys/save), oauth_device the device-code sign-in at
// the catalogue's routes, none a connection test alone, and document — a
// provider with a settings document but no bespoke panel here — the generic
// card ("configured on the server"). The panel's overrides (Azure AI Foundry,
// the local model, Copilot's PAT fallback) sit in front of this switch.
export default function CredentialForm(props: CredentialFormProps) {
  const {t} = useTranslation('settings')
  const {spec, onConfigured} = props
  if (!formSupported(spec)) {
    return <GenericProviderCard spec={spec} note={t('providers.unknownKind')} onConfigured={onConfigured} />
  }
  switch (spec.credential.kind) {
    case 'api_key':
      return <ApiKeyCredential spec={spec} onConfigured={onConfigured} />
    case 'oauth_device':
      return <DeviceFlowCredential {...props} />
    case 'none':
      return <TestOnly spec={spec} />
    default:
      return <GenericProviderCard spec={spec} onConfigured={onConfigured} />
  }
}

// ---- api_key ----

// A field's value by its catalogue name: an own key only, since a name such
// as `constructor` would otherwise read Object.prototype.
function fieldValue(values: Record<string, string>, name: string): string {
  return Object.prototype.hasOwnProperty.call(values, name) ? values[name]! : ''
}

function trimmed(values: Record<string, string>, name: string): string {
  return fieldValue(values, name).trim()
}

function ApiKeyCredential({spec, onConfigured}: {spec: ProviderSpec; onConfigured: () => void}) {
  const {t} = useTranslation('settings')
  const idBase = useId()
  const fields = spec.credential.fields
  // The first secret is the key; the rest ride along by name.
  const keyField = fields.find(f => f.type === 'secret')!
  const [values, setValues] = useState<Record<string, string>>({})
  // null until the key store has answered (see the effect below).
  const [hasKey, setHasKey] = useState<boolean | null>(null)
  const [replacing, setReplacing] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProviderTestResult | null>(null)
  // Where the key actually ends up: the OS keychain on a local backend, the
  // server (encrypted at rest) in cloud mode. Same signal the Privacy panel uses.
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()

  // The key store decides the row, not the catalogue: its `configured`
  // covers an organisation's shared credential too, and a stale "Remove"
  // would post a delete for a key that is not there. Nothing shows until it
  // answers; when it cannot be asked, the catalogue's word stands.
  useEffect(() => {
    let cancelled = false
    providersApi
      .hasApiKey(spec.id)
      .then(has => {
        if (!cancelled) setHasKey(has)
      })
      .catch(err => {
        logger.warn('Failed to check the stored key', err)
        if (!cancelled) setHasKey(spec.configured)
      })
    return () => {
      cancelled = true
    }
  }, [spec.id, spec.configured])

  const key = trimmed(values, keyField.name)
  // Every required field, and a key: a blank key on a configured provider
  // means "unchanged", and nothing is sent for it.
  const canSave = key !== '' && fields.every(f => !f.required || f === keyField || trimmed(values, f.name) !== '')

  const handleSave = async () => {
    if (!canSave) return
    setSaving(true)
    setSaveError(null)
    const extra: Record<string, string> = {}
    for (const f of fields) {
      if (f !== keyField && trimmed(values, f.name) !== '') extra[f.name] = trimmed(values, f.name)
    }
    try {
      if (Object.keys(extra).length > 0) await providersApi.saveApiKey(spec.id, key, extra)
      else await providersApi.saveApiKey(spec.id, key)
      setValues({})
      setHasKey(true)
      setReplacing(false)
      setTestResult(null)
      onConfigured()
    } catch (err) {
      setSaveError(credentialErrorText(t, err, t('apiKey.saveFailed')))
    } finally {
      setSaving(false)
    }
  }

  const handleRemove = async () => {
    setSaveError(null)
    try {
      await providersApi.deleteApiKey(spec.id)
      setHasKey(false)
      setReplacing(false)
      setTestResult(null)
      onConfigured()
    } catch (err) {
      logger.warn('Delete API key failed:', err)
      setSaveError(credentialErrorText(t, err, t('providers.removeFailed')))
    }
  }

  const handleTest = async () => {
    setTesting(true)
    setTestResult(null)
    try {
      setTestResult(await providersApi.testProviderConnection(spec.id))
    } catch (err) {
      setTestResult({ok: false, latencyMs: 0, error: getErrorMessage(err, ''), code: getErrorCode(err) ?? undefined})
    } finally {
      setTesting(false)
    }
  }

  const cancelReplace = () => {
    setReplacing(false)
    setValues({})
    setSaveError(null)
  }

  const errorLine = saveError && (
    <p role="alert" className="text-xs text-red-400">
      {saveError}
    </p>
  )

  if (hasKey === null) return null

  if (hasKey && !replacing) {
    return (
      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm text-text-secondary">
            {localBackend ? t('keys.configuredLocal') : t('keys.configuredServer')}
          </span>
          <Button size="sm" variant="secondary" onClick={handleRemove}>
            {t('keys.remove')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing}>
            {testing ? t('keys.testing') : t('keys.test')}
          </Button>
          <Button size="sm" variant="secondary" onClick={() => setReplacing(true)}>
            {t('providers.replaceKey')}
          </Button>
          {testResult && (
            <span className={testResult.ok ? 'text-xs text-green-400' : 'text-xs text-red-400'}>
              {testResultText(t, testResult)}
            </span>
          )}
        </div>
        {errorLine}
      </div>
    )
  }

  return (
    <div className="space-y-3">
      {fields.map(field => (
        <FieldInput
          key={field.name}
          id={`${idBase}-${field.name}`}
          field={field}
          value={fieldValue(values, field.name)}
          onChange={v => setValues(prev => ({...prev, [field.name]: v}))}
          placeholder={field === keyField && hasKey ? t('providers.replaceKeyPlaceholder') : field.placeholder}
        />
      ))}
      <div className="flex items-center gap-2">
        <Button size="sm" onClick={handleSave} disabled={!canSave || saving}>
          {saving ? t('apiKey.saving') : t('apiKey.save')}
        </Button>
        {hasKey && (
          <Button size="sm" variant="secondary" onClick={cancelReplace} disabled={saving}>
            {t('providers.formCancel')}
          </Button>
        )}
      </div>
      {errorLine}
    </div>
  )
}

// One field by its type. The types are closed here on purpose: formSupported
// keeps an unknown one out of the form, so the fallthrough renders nothing.
function FieldInput({
  id,
  field,
  value,
  onChange,
  placeholder,
}: {
  id: string
  field: CredentialField
  value: string
  onChange: (value: string) => void
  placeholder?: string
}) {
  switch (field.type) {
    case 'secret':
      return (
        <SecretField
          id={id}
          label={field.label}
          value={value}
          onChange={onChange}
          placeholder={placeholder}
          hint={field.help}
        />
      )
    case 'text':
    case 'url':
    case 'number':
      return (
        <FormField id={id} label={field.label}>
          <Input
            id={id}
            type={field.type}
            value={value}
            onChange={e => onChange(e.target.value)}
            placeholder={placeholder}
            hint={field.help}
            autoComplete="off"
            spellCheck={false}
          />
        </FormField>
      )
    case 'select':
      return (
        <FormField id={id} label={field.label}>
          <Select
            id={id}
            value={value}
            onChange={e => onChange(e.target.value)}
            options={field.options ?? []}
            placeholder={placeholder ?? ''}
            hint={field.help}
          />
        </FormField>
      )
    case 'json':
      return (
        <FormField id={id} label={field.label}>
          <Textarea
            id={id}
            value={value}
            onChange={e => onChange(e.target.value)}
            placeholder={placeholder}
            hint={field.help}
            spellCheck={false}
          />
        </FormField>
      )
    default:
      return null
  }
}

// ---- none ----

function TestOnly({spec}: {spec: ProviderSpec}) {
  const {t} = useTranslation('settings')
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProviderTestResult | null>(null)

  const handleTest = useCallback(async () => {
    setTesting(true)
    setTestResult(null)
    try {
      setTestResult(await providersApi.testProviderConnection(spec.id))
    } catch (err) {
      setTestResult({ok: false, latencyMs: 0, error: getErrorMessage(err, ''), code: getErrorCode(err) ?? undefined})
    } finally {
      setTesting(false)
    }
  }, [spec.id])

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-sm text-text-secondary">{t('providers.noCredential')}</span>
      <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing}>
        {testing ? t('keys.testing') : t('keys.test')}
      </Button>
      {testResult && (
        <span className={testResult.ok ? 'text-xs text-green-400' : 'text-xs text-red-400'}>
          {testResultText(t, testResult)}
        </span>
      )}
    </div>
  )
}

// ---- oauth_device ----

// The sign-in button's colours: Tailwind needs the classes spelled out, so a
// provider without an entry gets the brand colour.
const DEVICE_BUTTON_CLASS: Record<string, string> = {
  'github-models': 'bg-[#24292e] hover:bg-[#2f363d]',
  copilot: 'bg-[#6e40c9] hover:bg-[#5a32a3]',
}

/**
 * The device-code flow bound to the catalogue's routes. One `user` GET per
 * mount (the button's own), reported through `onUser` so the host needs no
 * second one.
 */
export function DeviceFlowCredential({spec, onConfigured, onSignedIn, onUser}: CredentialFormProps) {
  const {t} = useTranslation('settings')
  const {t: tChat} = useTranslation('chat')
  // The latest callbacks, so the provider object (which the button re-reads
  // the user for whenever it changes) depends on the routes alone.
  const callbacks = useRef({onConfigured, onSignedIn, onUser})
  useEffect(() => {
    callbacks.current = {onConfigured, onSignedIn, onUser}
  }, [onConfigured, onSignedIn, onUser])
  const start = apiPath(spec.endpoints.start)
  const poll = apiPath(spec.endpoints.poll)
  const revoke = apiPath(spec.endpoints.revoke)
  const user = apiPath(spec.endpoints.user)
  const github = spec.id === 'github-models' || spec.id === 'copilot'

  const provider = useMemo<DeviceAuthProvider>(
    () => ({
      name: spec.id,
      label: github ? tChat('auth.signInWithGitHub') : t('providers.signInWith', {name: spec.name}),
      buttonClassName: DEVICE_BUTTON_CLASS[spec.id] ?? 'bg-brand-500 hover:bg-brand-600',
      // The catalogue's glyph; without one the GitHub ids keep their mark
      // and anyone else gets a key.
      icon: spec.icon ?? (github ? 'github' : 'key'),
      getUser: () =>
        user
          ? providersApi.getDeviceAuthUser(user).then(u => {
              callbacks.current.onUser?.(u)
              return u
            })
          : Promise.resolve(null),
      startAuth: () => providersApi.startDeviceAuth(start!),
      pollAuth: deviceCode => providersApi.pollDeviceAuth(poll!, deviceCode),
      revokeAuth: () =>
        providersApi.revokeDeviceAuth(revoke!).then(() => {
          callbacks.current.onUser?.(null)
          callbacks.current.onConfigured()
        }),
    }),
    [spec.id, spec.name, spec.icon, github, start, poll, revoke, user, t, tChat],
  )

  const onAuthComplete = useCallback(() => {
    callbacks.current.onConfigured()
    callbacks.current.onSignedIn?.()
  }, [])

  return <DeviceAuthLoginButton provider={provider} onAuthComplete={onAuthComplete} />
}
