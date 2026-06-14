import {useState, useCallback, useEffect} from 'react'
import Input from '@/components/shared/Input'
import Button from '@/components/shared/Button'
import {providersApi} from '@/api'
import type {ProviderTestResult} from '@/types'
import {logger} from '@/lib/logger'

interface Props {
  provider: string
  label: string
  onConfigured?: () => void
}

export default function ApiKeyInput({provider, label, onConfigured}: Props) {
  const [key, setKey] = useState('')
  const [showKey, setShowKey] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<'valid' | 'invalid' | null>(null)
  const [hasKey, setHasKey] = useState<boolean | null>(null)

  const checkKey = useCallback(() => {
    providersApi.hasApiKey(provider).then(setHasKey).catch(() => setHasKey(false))
  }, [provider])

  useEffect(() => { checkKey() }, [checkKey])

  const handleSave = useCallback(async () => {
    if (!key.trim()) return
    setSaving(true)
    setSaveError(false)
    try {
      await providersApi.saveApiKey(provider, key.trim())
      setKey('')
      setHasKey(true)
      setTestResult(null)
      onConfigured?.()
    } catch {
      setSaveError(true)
    } finally {
      setSaving(false)
    }
  }, [key, provider, onConfigured])

  const handleDelete = useCallback(async () => {
    try {
      await providersApi.deleteApiKey(provider)
      setHasKey(false)
      setTestResult(null)
      onConfigured?.()
    } catch (e) { logger.warn('Delete API key failed:', e) }
  }, [provider, onConfigured])

  const handleTest = useCallback(async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const result = await providersApi.testProviderConnection(provider) as ProviderTestResult
      setTestResult(result?.ok ? 'valid' : 'invalid')
    } catch {
      setTestResult('invalid')
    } finally {
      setTesting(false)
    }
  }, [provider])

  return (
    <div className="space-y-2">
      <label className="text-sm font-medium text-text-primary">{label}</label>
      {hasKey ? (
        <div className="flex items-center gap-2">
          <span className="text-sm text-text-secondary">API key configured (saved in keychain)</span>
          <Button size="sm" variant="secondary" onClick={handleDelete}>Remove</Button>
          <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing}>
            {testing ? 'Testing...' : 'Test'}
          </Button>
          {testResult === 'valid' && <span className="text-xs text-green-400">Valid</span>}
          {testResult === 'invalid' && <span className="text-xs text-red-400">Invalid</span>}
        </div>
      ) : (
        <div className="flex items-center gap-2">
          <Input
            type={showKey ? 'text' : 'password'}
            value={key}
            onChange={(e) => setKey((e.target as HTMLInputElement).value)}
            placeholder="Enter API key..."
            className="flex-1"
          />
          <Button size="sm" variant="secondary" onClick={() => setShowKey(!showKey)}>
            {showKey ? 'Hide' : 'Show'}
          </Button>
          <Button size="sm" onClick={handleSave} disabled={!key.trim() || saving}>
            {saving ? 'Saving...' : 'Save'}
          </Button>
          {saveError && <span className="text-xs text-red-400">Failed to save</span>}
        </div>
      )}
    </div>
  )
}
