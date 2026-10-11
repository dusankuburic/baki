import {getErrorMessage} from '@/lib/getErrorMessage'
import React, {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {History, Plus, Tag, RotateCcw, GitCompare} from 'lucide-react'
import {versionsApi, type FlowVersion} from '@/api/admin'
import {libraryApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useToast, useConfirm} from '@/components/shared'
import {EmptyState, ErrorState, Spinner} from '@/components/shared'
import {relativeTime, absoluteTime} from '@/lib/time'
import {useAsync} from '@/hooks/useAsync'
import type {FlowDiff} from '@/types'

export const HistoryTab: React.FC = () => {
  const {t} = useTranslation('inspector')
  const document = useFlowStore(s => s.document)
  const setDocument = useFlowStore(s => s.setDocument)
  const setActiveDiff = useUIStore(s => s.setActiveDiff)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const toast = useToast()
  const {confirm} = useConfirm()

  const [isSaving, setIsSaving] = useState(false)
  const [comment, setComment] = useState('')
  const [saveError, setSaveError] = useState<string | null>(null)
  const [busyVersion, setBusyVersion] = useState<number | null>(null)

  const {
    data,
    isLoading,
    error: fetchError,
    refetch: fetchVersions,
  } = useAsync<FlowVersion[]>(
    () => (document?.id ? versionsApi.list(document.id, 50) : Promise.resolve([])),
    [document?.id],
  )
  const versions = fetchError ? [] : (data ?? [])
  const error = saveError ?? (fetchError ? t('errors.loadVersionsFailed') : null)

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!document?.id) return
    setIsSaving(true)
    setSaveError(null)
    try {
      await versionsApi.save(document.id, comment)
      setComment('')
      fetchVersions()
    } catch (err) {
      setSaveError(getErrorMessage(err, t('errors.saveVersionFailed')))
    } finally {
      setIsSaving(false)
    }
  }

  const handleCompare = async (v: FlowVersion) => {
    if (!document?.id || busyVersion !== null) return
    setBusyVersion(v.version)
    try {
      const diff = (await versionsApi.diff(document.id, v.version)) as FlowDiff | null
      if (diff) {
        setActiveDiff(diff)
        setMainPaneView('diff')
      }
    } catch (err) {
      toast.error(t('toasts.comparisonFailed'), {description: err instanceof Error ? err.message : undefined})
    } finally {
      setBusyVersion(null)
    }
  }

  const handleRestore = async (v: FlowVersion) => {
    if (!document?.id || busyVersion !== null) return
    const ok = await confirm({
      title: t('restore.title'),
      message: t('restore.message', {version: v.version}),
      danger: true,
      confirmLabel: t('restore.confirm'),
    })
    if (!ok) return
    setBusyVersion(v.version)
    try {
      await versionsApi.restore(document.id, v.version)
      const refreshed = await libraryApi.getContent(document.id)
      setDocument(refreshed)
      fetchVersions()
      toast.success(t('toasts.restored'), {description: t('toasts.revertedTo', {version: v.version})})
    } catch (err) {
      toast.error(t('toasts.restoreFailed'), {description: err instanceof Error ? err.message : undefined})
    } finally {
      setBusyVersion(null)
    }
  }

  if (!document) {
    return (
      <div className="p-8 text-center text-text-tertiary">
        <History size={32} className="mx-auto mb-2 opacity-20" />
        <p className="text-sm">{t('history.emptyNoFlow')}</p>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full bg-surface-1 overflow-y-auto">
      <div className="p-4 border-b border-border-subtle bg-surface-2/50">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3 flex items-center gap-1.5">
          <Plus size={13} />
          {t('history.saveHeading')}
        </h3>
        <form onSubmit={handleSave} className="flex gap-2">
          <input
            type="text"
            placeholder={t('history.commentPlaceholder')}
            aria-label={t('history.commentPlaceholder')}
            value={comment}
            onChange={e => setComment(e.target.value)}
            aria-invalid={!!saveError || undefined}
            aria-describedby={error ? 'history-save-error' : undefined}
            className="flex-1 bg-surface-0 border border-border-default rounded-md px-3 py-1.5 text-xs focus:outline-hidden focus:ring-2 focus:ring-brand-500/50"
          />
          <button
            type="submit"
            disabled={isSaving}
            className="px-3 py-1.5 bg-brand-600 text-brand-foreground rounded-md text-xs font-semibold hover:bg-brand-700 disabled:opacity-50 transition-colors"
          >
            {isSaving ? t('history.saving') : t('history.save')}
          </button>
        </form>
        {error && (
          <p id="history-save-error" role="alert" className="text-2xs text-semantic-error mt-1">
            {error}
          </p>
        )}
      </div>

      <div className="flex-1 p-4">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3">{t('history.heading')}</h3>
        {isLoading ? (
          <div className="flex justify-center p-8">
            <Spinner size={20} />
          </div>
        ) : versions.length === 0 ? (
          error ? (
            <ErrorState message={error} onRetry={fetchVersions} />
          ) : (
            <EmptyState title={t('history.emptyTitle')} description={t('history.emptyDescription')} />
          )
        ) : (
          <div className="space-y-2">
            {versions.map(v => {
              const busy = busyVersion === v.version
              return (
                <div
                  key={v.id}
                  className="flex items-start gap-3 p-2.5 rounded-lg bg-surface-2 border border-border-subtle/50"
                >
                  <div className="flex flex-col items-center gap-1 shrink-0">
                    <Tag size={13} className="text-text-tertiary" />
                    <span className="text-2xs text-text-tertiary font-mono">v{v.version}</span>
                  </div>
                  <div className="flex-1 min-w-0">
                    <p className="text-xs text-text-primary truncate">
                      {v.comment || <span className="text-text-tertiary italic">{t('history.noComment')}</span>}
                    </p>
                    <p className="text-2xs text-text-tertiary mt-0.5">
                      <span title={absoluteTime(v.createdAt)}>{relativeTime(v.createdAt)}</span>
                      {v.metadata?.blockCount != null && (
                        <span className="ml-2 text-text-tertiary">
                          {t('history.blockCount', {count: v.metadata.blockCount})}
                        </span>
                      )}
                    </p>
                  </div>
                  <div className="flex flex-col gap-1 shrink-0">
                    <button
                      type="button"
                      title={t('history.compareTitle', {version: v.version})}
                      aria-label={t('history.compareTitle', {version: v.version})}
                      disabled={busy}
                      onClick={() => handleCompare(v)}
                      className="p-1 rounded text-text-tertiary hover:text-text-primary hover:bg-surface-0 disabled:opacity-40 transition-colors"
                    >
                      {busy ? <Spinner size={13} /> : <GitCompare size={13} />}
                    </button>
                    <button
                      type="button"
                      title={t('history.restoreTitle', {version: v.version})}
                      aria-label={t('history.restoreTitle', {version: v.version})}
                      disabled={busy}
                      onClick={() => handleRestore(v)}
                      className="p-1 rounded text-text-tertiary hover:text-text-primary hover:bg-surface-0 disabled:opacity-40 transition-colors"
                    >
                      <RotateCcw size={13} />
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}
