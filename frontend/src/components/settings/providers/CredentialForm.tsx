import {useCallback, useEffect, useId, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Button, FormField, Input, SecretField, Select, Textarea} from '@/components/shared'
import DeviceAuthLoginButton, {DEVICE_BUTTON_CLASS, type DeviceAuthProvider} from '@/components/chat/DeviceAuthLoginButton'
import {providersApi} from '@/api'
import {apiPath} from '@/api/providers'
import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'
import {CREDENTIAL_FIELD_TYPES, CREDENTIAL_KINDS, isKnown} from '@/types'
import {API_KEY_MAX_LENGTH} from '@/lib/constraints'
import type {CredentialField, GitHubUser, ProviderSpec, ProviderTestResult} from '@/types'
import GenericProviderCard, {credentialErrorText, testResultText} from './GenericProviderCard'

export interface CredentialFormProps {
  spec: ProviderSpec
  onConfigured: () => void
  onSignedIn?: () => void
  onUser?: (user: GitHubUser | null) => void
}

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
  const keyField = fields.find(f => f.type === 'secret')!
  const [values, setValues] = useState<Record<string, string>>({})
  const [hasKey, setHasKey] = useState<boolean | null>(null)
  const [replacing, setReplacing] = useState(false)
  // Stored, but the server cannot decrypt it (its encryption key changed): only entering it again repairs it.
  const [unreadable, setUnreadable] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProviderTestResult | null>(null)
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()

  useEffect(() => {
    let cancelled = false
    providersApi
      .keyStatus(spec.id)
      .then(status => {
        if (cancelled) return
        setHasKey(status.has)
        setUnreadable(status.has && status.unreadable)
      })
      .catch(err => {
        logger.warn('Failed to check the stored key', err)
        if (cancelled) return
        const unreadableInCatalog = spec.credentialStatus === 'unreadable'
        setHasKey(spec.configured || unreadableInCatalog)
        setUnreadable(unreadableInCatalog)
      })
    return () => {
      cancelled = true
    }
  }, [spec.id, spec.configured, spec.credentialStatus])

  const key = trimmed(values, keyField.name)
  const canSave = key !== '' && fields.every(f => !f.required || f === keyField || trimmed(values, f.name) !== '')

  const fieldNames = useMemo(() => fields.map(f => f.name), [fields])
  const v = useFormValidation<string>({
    fields: fieldNames,
    validate: () => {
      const errors: FieldErrors<string> = {}
      if (key === '') errors[keyField.name] = t('errors.required', {ns: 'common'})
      for (const f of fields) {
        if (f.required && f !== keyField && trimmed(values, f.name) === '')
          errors[f.name] = t('errors.required', {ns: 'common'})
      }
      return errors
    },
  })

  const handleSave = async () => {
    if (v.submitValidate()) return
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
      v.reset()
      setHasKey(true)
      setUnreadable(false)
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
      setUnreadable(false)
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
    v.reset()
  }

  const errorLine = saveError && (
    <p role="alert" className="text-xs text-red-400">
      {saveError}
    </p>
  )

  if (hasKey === null) return null

  if (hasKey && unreadable && !replacing) {
    return (
      <div className="space-y-2">
        <p className="text-sm text-semantic-warning">{t('keys.unreadable')}</p>
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" onClick={() => setReplacing(true)}>
            {t('keys.enterAgain')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleRemove}>
            {t('keys.remove')}
          </Button>
        </div>
        {errorLine}
      </div>
    )
  }

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
          onChange={next => setValues(prev => ({...prev, [field.name]: next}))}
          onBlur={() => v.touch(field.name)}
          error={v.errorFor(field.name)}
          placeholder={field === keyField && hasKey ? t('providers.replaceKeyPlaceholder') : field.placeholder}
          maxLength={field === keyField ? API_KEY_MAX_LENGTH : undefined}
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

function FieldInput({
  id,
  field,
  value,
  onChange,
  onBlur,
  error,
  placeholder,
  maxLength,
}: {
  id: string
  field: CredentialField
  value: string
  onChange: (value: string) => void
  onBlur?: () => void
  error?: string
  placeholder?: string
  maxLength?: number
}) {
  switch (field.type) {
    case 'secret':
      return (
        <SecretField
          id={id}
          label={field.label}
          value={value}
          onChange={onChange}
          onBlur={onBlur}
          placeholder={placeholder}
          hint={field.help}
          error={error}
          maxLength={maxLength}
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
            onBlur={onBlur}
            placeholder={placeholder}
            hint={field.help}
            error={error}
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
            onBlur={onBlur}
            options={(field.options ?? []).map(o => ({value: o.value, label: o.label || o.value}))}
            placeholder={placeholder ?? ''}
            hint={field.help}
            error={error}
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
            onBlur={onBlur}
            placeholder={placeholder}
            hint={field.help}
            error={error}
            spellCheck={false}
          />
        </FormField>
      )
    default:
      return null
  }
}

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

export function DeviceFlowCredential({spec, onConfigured, onSignedIn, onUser}: CredentialFormProps) {
  const {t} = useTranslation('settings')
  const {t: tChat} = useTranslation('chat')
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
