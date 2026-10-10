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
  /** Why the form could not be rendered, when that is the reason for the card (an unknown kind or field type). */
  note?: string
  /** The credential was removed: the host re-checks the providers. */
  onConfigured: () => void
}

/**
 * A refused save or removal, worded for the user: the server's code when the
 * bundle has copy for it, else the server's message with the code, else the
 * message alone, else the caller's fallback (no envelope at all — a raw
 * TypeError's text is not for the user).
 */
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

/** "Valid · 12 ms", "Valid", or "Invalid · <error> (<code>)". */
export function testResultText(t: TFunction<'settings'>, result: ProviderTestResult): string {
  if (result.ok) return result.latencyMs > 0 ? t('apiKey.validLatency', {ms: result.latencyMs}) : t('apiKey.valid')
  const detail = result.error && result.code ? `${result.error} (${result.code})` : result.error || result.code
  return detail ? t('providers.testFailedDetail', {detail}) : t('apiKey.invalid')
}

/** A link target the card may offer: http(s) only, never a scheme the server could abuse. */
export function helpHref(url: string | undefined): string | undefined {
  if (!url) return undefined
  try {
    const u = new URL(url)
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.href : undefined
  } catch {
    return undefined
  }
}

// The section body for a provider the bundle cannot form: one it does not
// know (a `document` kind without a bespoke panel), or one whose credential
// kind or field type a newer server added. It shows what is safe to show —
// the configured state, the help link, Test and Remove when configured —
// and never guesses an input: a wrong widget could echo a secret or send a
// malformed payload.
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

  // The generic removal route; a document provider answers USE_PROVIDER_CONFIG, shown as such.
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
