import {getErrorMessage, getErrorCode} from '@/lib/getErrorMessage'
import {useState, useCallback, useEffect, useId, useRef} from 'react'
import {useTranslation} from 'react-i18next'
import Input from '@/components/shared/Input'
import Button from '@/components/shared/Button'
import Skeleton from '@/components/shared/Skeleton'
import {useConfirm} from '@/components/shared'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import type {LocalModelsConfig, ProviderTestResult} from '@/types'
import {LOCAL_DEFAULT_CONTEXT_WINDOW, LOCAL_MAX_CONTEXT_WINDOW, LOCAL_MIN_CONTEXT_WINDOW} from '@/types/providers'
import {formatCount} from '@/lib/format'
import {logger} from '@/lib/logger'

// The generic local OpenAI-compatible provider (Foundry Local, Ollama, LM
// Studio, ...) is configured with a base URL — no key — and an optional
// context window, so it gets its own small section. Desktop only: a cloud
// server answers LOCAL_MODELS_UNAVAILABLE, and the section then renders
// nothing (and tells its host, which drops the heading). The server is
// authoritative on both values; the client checks only catch an obviously
// malformed value early.

const PROVIDER_ID = 'local'

interface Props {
  /** Called after a save or removal so the host can refresh provider state. */
  onConfigured?: () => void
  /** Called once when the server says local models are not available (cloud). */
  onUnavailable?: () => void
}

type BaseUrlErrorKey = 'providers.localBaseUrlInvalid'

// An absolute http(s) URL with a host. Where it may point is the server's
// call (INVALID_BASE_URL).
function baseUrlErrorKey(value: string): BaseUrlErrorKey | null {
  const v = value.trim()
  if (!v) return null
  try {
    const u = new URL(v)
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.hostname ? null : 'providers.localBaseUrlInvalid'
  } catch {
    return 'providers.localBaseUrlInvalid'
  }
}

type ContextWindowErrorKey = 'providers.localContextWindowInvalid'

// Blank is the server's default (8192); anything else must be a whole number
// of tokens within the server's bounds (INVALID_CONTEXT_WINDOW otherwise).
function contextWindowErrorKey(value: string): ContextWindowErrorKey | null {
  const v = value.trim()
  if (!v) return null
  if (!/^\d+$/.test(v)) return 'providers.localContextWindowInvalid'
  const n = Number(v)
  return n >= LOCAL_MIN_CONTEXT_WINDOW && n <= LOCAL_MAX_CONTEXT_WINDOW ? null : 'providers.localContextWindowInvalid'
}

// The saved window as the field shows it: blank when unset.
const savedContextWindow = (cfg: LocalModelsConfig | null): string =>
  cfg?.contextWindow ? String(cfg.contextWindow) : ''

const isUnavailable = (err: unknown): boolean => getErrorCode(err) === 'LOCAL_MODELS_UNAVAILABLE'

export default function LocalModelsSection({onConfigured, onUnavailable}: Props) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  // null while the first GET is in flight.
  const [config, setConfig] = useState<LocalModelsConfig | null>(null)
  const [loadFailed, setLoadFailed] = useState(false)
  // The server does not offer local models (cloud deployment): render nothing.
  const [unavailable, setUnavailable] = useState(false)
  // A save or remove succeeded but the reload of the view failed: say so (with
  // a retry) instead of showing the pre-change view.
  const [changedButStale, setChangedButStale] = useState(false)
  const [editing, setEditing] = useState(false)
  const [baseUrl, setBaseUrl] = useState('')
  const [touched, setTouched] = useState(false)
  const [contextWindow, setContextWindow] = useState('')
  const [contextWindowTouched, setContextWindowTouched] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ok: boolean; latencyMs?: number; error?: string} | null>(null)
  const [removing, setRemoving] = useState(false)
  const [removeError, setRemoveError] = useState<string | null>(null)
  // Held as a skeleton while a save/remove waits on the reload of the view, so
  // the stale (or empty) view cannot flash.
  const [refreshing, setRefreshing] = useState(false)
  // The form/summary swap unmounts whatever had focus (Save, Remove, Cancel,
  // Edit) and focus falls to <body>: pendingFocus names the view to restore it
  // into once it settles ('summary' -> Edit, 'form' -> the base URL field).
  const rootRef = useRef<HTMLDivElement>(null)
  const [pendingFocus, setPendingFocus] = useState<'summary' | 'form' | null>(null)
  // Read through a ref so a host's inline callback cannot re-run the load.
  const onUnavailableRef = useRef(onUnavailable)
  useEffect(() => {
    onUnavailableRef.current = onUnavailable
  })

  const markUnavailable = useCallback(() => {
    setUnavailable(true)
    onUnavailableRef.current?.()
  }, [])

  // Resolves true when the view was (re)loaded.
  const load = useCallback(
    (): Promise<boolean> =>
      providersApi
        .getLocalModelsConfig()
        .then(cfg => {
          setConfig(cfg)
          setLoadFailed(false)
          setChangedButStale(false)
          return true
        })
        .catch(err => {
          if (isUnavailable(err)) {
            markUnavailable()
            return false
          }
          // A failed GET must not render as "not configured": an error row
          // with a retry shows instead of the empty form.
          logger.warn('Failed to load local model config', err)
          setLoadFailed(true)
          return false
        }),
    [markUnavailable],
  )

  useEffect(() => {
    void load()
  }, [load])

  const configured = config?.configured === true
  const showForm = config !== null && (!configured || editing)
  const urlErr = baseUrlErrorKey(baseUrl)
  const windowErr = contextWindowErrorKey(contextWindow)
  const canSave = baseUrl.trim() !== '' && urlErr === null && windowErr === null && !saving

  const uid = useId()
  const baseUrlId = `${uid}-base-url`
  const contextWindowId = `${uid}-context-window`

  const resetForm = useCallback(() => {
    setBaseUrl('')
    setTouched(false)
    setContextWindow('')
    setContextWindowTouched(false)
    setSaveError(null)
  }, [])

  const startEdit = () => {
    setBaseUrl(config?.baseUrl ?? '')
    setTouched(false)
    setContextWindow(savedContextWindow(config))
    setContextWindowTouched(false)
    setSaveError(null)
    setTestResult(null)
    setRemoveError(null)
    setEditing(true)
    setPendingFocus('form')
  }

  const cancelEdit = useCallback(async () => {
    if (baseUrl.trim() !== (config?.baseUrl ?? '') || contextWindow.trim() !== savedContextWindow(config)) {
      const ok = await confirm({
        title: t('providers.localDiscardTitle'),
        message: t('providers.localDiscardMessage'),
        danger: true,
        confirmLabel: t('providers.localDiscard'),
      })
      if (!ok) return
    }
    setPendingFocus('summary')
    setEditing(false)
    resetForm()
  }, [baseUrl, contextWindow, config, confirm, resetForm, t])

  const handleSave = useCallback(async () => {
    setSaving(true)
    setSaveError(null)
    try {
      // Blank sends no window: the server applies its default.
      const tokens = contextWindow.trim()
      await providersApi.saveLocalModelsConfig(baseUrl.trim(), tokens ? Number(tokens) : undefined)
      setEditing(false)
      resetForm()
      setTestResult(null)
      setRemoveError(null)
      setPendingFocus('summary')
      setRefreshing(true)
      const reloaded = await load()
      setRefreshing(false)
      if (!reloaded) setChangedButStale(true)
      onConfigured?.()
    } catch (err) {
      if (isUnavailable(err)) {
        markUnavailable()
      } else if (err instanceof ApiError) {
        // INVALID_BASE_URL and INVALID_CONTEXT_WINDOW carry a message that
        // says what is wrong with the value; keep it.
        setSaveError(err.message || t('providers.localSaveFailed'))
      } else {
        logger.warn('Save local model config failed:', err)
        setSaveError(t('providers.localSaveFailed'))
      }
    } finally {
      setSaving(false)
    }
  }, [baseUrl, contextWindow, load, markUnavailable, onConfigured, resetForm, t])

  const handleRemove = useCallback(async () => {
    const ok = await confirm({
      title: t('providers.localRemoveTitle'),
      message: t('providers.localRemoveMessage'),
      danger: true,
      confirmLabel: t('keys.remove'),
    })
    if (!ok) return
    setRemoving(true)
    setRemoveError(null)
    try {
      await providersApi.deleteApiKey(PROVIDER_ID)
      setTestResult(null)
      setEditing(false)
      resetForm()
      setPendingFocus('form')
      setRefreshing(true)
      const reloaded = await load()
      setRefreshing(false)
      if (!reloaded) setChangedButStale(true)
      onConfigured?.()
    } catch (err) {
      logger.warn('Remove local model config failed:', err)
      setRemoveError(
        err instanceof ApiError ? err.message || t('providers.localRemoveFailed') : t('providers.localRemoveFailed'),
      )
    } finally {
      setRemoving(false)
    }
  }, [confirm, load, onConfigured, resetForm, t])

  const handleTest = useCallback(async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const result: ProviderTestResult | null = await providersApi.testProviderConnection(PROVIDER_ID)
      // Local failures are actionable (server not running, model not pulled):
      // keep the server's text. A null body carries no reason.
      if (result?.ok) {
        setTestResult({ok: true, latencyMs: result.latencyMs})
      } else {
        setTestResult({ok: false, error: result?.error || t('providers.localTestFailed')})
      }
    } catch (err) {
      setTestResult({ok: false, error: getErrorMessage(err, t('providers.localTestFailed'))})
    } finally {
      setTesting(false)
    }
  }, [t])

  // Runs once the view that replaced the focused one settled and moves focus
  // to it; cleared even when the target is absent. useId ids contain colons,
  // hence the CSS.escape selector.
  useEffect(() => {
    if (!pendingFocus || refreshing || config === null) return
    const root = rootRef.current
    const target =
      pendingFocus === 'summary'
        ? root?.querySelector<HTMLElement>('[data-action="edit"]')
        : root?.querySelector<HTMLElement>(`#${CSS.escape(baseUrlId)}`)
    target?.focus()
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setPendingFocus(null)
  }, [pendingFocus, refreshing, config, baseUrlId])

  if (unavailable) return null

  if (config === null && !loadFailed) {
    return <Skeleton lines={2} className="max-w-md" />
  }

  // A failed load is never "not configured": an error row with a retry, and
  // (when an earlier view exists) that view underneath it.
  const loadErrorRow = (
    <div role="alert" className="flex items-center gap-2 flex-wrap">
      <span className="text-xs text-red-400">{t('providers.localLoadFailed')}</span>
      <Button size="sm" variant="secondary" onClick={() => void load()} disabled={removing}>
        {t('providers.localRetry')}
      </Button>
    </div>
  )
  if (config === null && !refreshing) {
    return <div className="space-y-3 max-w-2xl">{loadErrorRow}</div>
  }

  if (refreshing || config === null) {
    return <Skeleton lines={2} className="max-w-md" />
  }

  // Saved (or removed), but the fresh view did not load: neither the old view
  // nor an empty form may stand in for it.
  if (changedButStale) {
    return (
      <div role="status" className="flex items-center gap-2 flex-wrap max-w-2xl">
        <span className="text-xs text-text-secondary">{t('providers.localSavedRefreshFailed')}</span>
        <Button size="sm" variant="secondary" onClick={() => void load()}>
          {t('providers.localRetry')}
        </Button>
      </div>
    )
  }

  if (!showForm && config.configured) {
    return (
      <div className="space-y-3" ref={rootRef}>
        {loadFailed && loadErrorRow}
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
          <dt className="text-text-tertiary">{t('providers.localConfiguredBaseUrl')}</dt>
          <dd>
            <code className="font-mono text-xs text-text-primary break-all">{config.baseUrl}</code>
          </dd>
          {/* The window history is trimmed to: the saved one, else the
              server's default. */}
          <dt className="text-text-tertiary">{t('providers.localConfiguredContextWindow')}</dt>
          <dd className="text-text-primary tabular-nums">
            {formatCount(config.contextWindow || LOCAL_DEFAULT_CONTEXT_WINDOW)}
          </dd>
        </dl>
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-sm text-text-secondary">{t('providers.localConfigured')}</span>
          <Button size="sm" variant="secondary" onClick={startEdit} disabled={removing} data-action="edit">
            {t('providers.localEdit')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleRemove} disabled={removing}>
            {removing ? t('keys.removing') : t('keys.remove')}
          </Button>
          <Button size="sm" variant="secondary" onClick={handleTest} disabled={testing || removing}>
            {testing ? t('keys.testing') : t('keys.test')}
          </Button>
          {/* Announced to assistive technology: the status region stays
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
      </div>
    )
  }

  return (
    <div className="space-y-3 max-w-2xl" ref={rootRef}>
      {loadFailed && loadErrorRow}
      <div className="space-y-1">
        <label htmlFor={baseUrlId} className="block text-xs font-medium text-text-secondary">
          {t('providers.localBaseUrl')}
        </label>
        <Input
          id={baseUrlId}
          value={baseUrl}
          onChange={e => setBaseUrl(e.target.value)}
          onBlur={() => setTouched(true)}
          placeholder={t('providers.localBaseUrlPlaceholder')}
          hint={t('providers.localBaseUrlHint')}
          error={touched && urlErr ? t(urlErr) : undefined}
          type="url"
          autoComplete="off"
          spellCheck={false}
        />
      </div>
      <div className="space-y-1">
        <label htmlFor={contextWindowId} className="block text-xs font-medium text-text-secondary">
          {t('providers.localContextWindow')}
        </label>
        {/* A text field with the numeric keypad rather than type="number":
            no spinner stepping a five-digit value by one, and a non-numeric
            entry stays visible (and flagged) instead of reading as blank. */}
        <Input
          id={contextWindowId}
          value={contextWindow}
          onChange={e => setContextWindow(e.target.value)}
          onBlur={() => setContextWindowTouched(true)}
          placeholder={t('providers.localContextWindowPlaceholder')}
          hint={t('providers.localContextWindowHint')}
          error={contextWindowTouched && windowErr ? t(windowErr) : undefined}
          inputMode="numeric"
          autoComplete="off"
          spellCheck={false}
        />
      </div>
      <div className="flex items-center gap-2 flex-wrap">
        <Button size="sm" variant="primary" onClick={handleSave} disabled={!canSave}>
          {saving ? t('providers.localSaving') : t('providers.localSave')}
        </Button>
        {editing && (
          <Button size="sm" variant="secondary" onClick={cancelEdit} disabled={saving}>
            {t('providers.localCancel')}
          </Button>
        )}
        {saveError && (
          <span role="alert" className="text-xs text-red-400">
            {saveError}
          </span>
        )}
      </div>
    </div>
  )
}
