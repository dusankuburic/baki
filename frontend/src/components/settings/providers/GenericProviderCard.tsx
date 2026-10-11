import {useCallback, useState} from 'react'
import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
import {Badge, Button} from '@/components/shared'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import type {ProviderSpec, ProviderTestResult} from '@/types'

interface Props {
  spec: ProviderSpec
  note?: string
  onConfigured: () => void
}

export function credentialErrorText(t: TFunction<'settings'>, err: unknown, fallback: string): string {
  const code = getErrorCode(err)
  if (code === 'PROVIDER_UNKNOWN')
    return t('providers.errorWithCode', {message: t('providers.codeProviderUnknown'), code})
  if (code === 'USE_PROVIDER_CONFIG') {
    return t('providers.errorWithCode', {message: t('providers.codeUseProviderConfig'), code})
  }
  const message = err instanceof ApiError ? err.message : ''
  if (code) return t('providers.errorWithCode', {message: message || fallback, code})
  return message || fallback
}

export function testResultText(t: TFunction<'settings'>, result: ProviderTestResult): string {
  if (result.ok) return result.latencyMs > 0 ? t('apiKey.validLatency', {ms: result.latencyMs}) : t('apiKey.valid')
  const detail = result.error && result.code ? `${result.error} (${result.code})` : result.error || result.code
  return detail ? t('providers.testFailedDetail', {detail}) : t('apiKey.invalid')
}

export function helpHref(url: string | undefined): string | undefined {
  if (!url) return undefined
  try {
    const u = new URL(url)
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.href : undefined
  } catch {
    return undefined
  }
}

export default function GenericProviderCard({spec, note, onConfigured}: Props) {
  const {t} = useTranslation('settings')
  const [configured, setConfigured] = useState(spec.configured)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProviderTestResult | null>(null)
  const [removing, setRemoving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const href = helpHref(spec.credential.helpUrl)

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

  const handleRemove = useCallback(async () => {
    setRemoving(true)
    setError(null)
    try {
      await providersApi.deleteApiKey(spec.id)
      setConfigured(false)
      setTestResult(null)
      onConfigured()
    } catch (err) {
      setError(credentialErrorText(t, err, t('providers.removeFailed')))
    } finally {
      setRemoving(false)
    }
  }, [spec.id, onConfigured, t])

  return (
    <div className="space-y-2" data-testid={`generic-provider-${spec.id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={configured ? 'success' : 'neutral'}>
          {configured ? t('providers.genericConfigured') : t('providers.genericNotConfigured')}
        </Badge>
        {configured && (
          <>
            <Button size="sm" variant="secondary" onClick={handleRemove} disabled={removing}>
              {removing ? t('keys.removing') : t('keys.remove')}
            </Button>
            <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing}>
              {testing ? t('keys.testing') : t('keys.test')}
            </Button>
            {testResult && (
              <span className={testResult.ok ? 'text-xs text-green-400' : 'text-xs text-red-400'}>
                {testResultText(t, testResult)}
              </span>
            )}
          </>
        )}
      </div>
      {note && <p className="text-xs text-text-tertiary">{note}</p>}
      <p className="text-xs text-text-tertiary">
        {configured ? t('providers.genericConfiguredHint') : t('providers.genericNewerVersion')}
      </p>
      {href && (
        <a href={href} target="_blank" rel="noopener noreferrer" className="text-xs text-brand-400 hover:underline">
          {t('providers.genericHelpLink')}
        </a>
      )}
      {error && (
        <p role="alert" className="text-xs text-red-400">
          {error}
        </p>
      )}
    </div>
  )
}
