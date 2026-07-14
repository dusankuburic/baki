import React, {useState} from 'react'
import {History, Plus, Tag} from 'lucide-react'
import {versionsApi, type FlowVersion} from '@/api/admin'
import {useFlowStore} from '@/stores/flowStore'
import {EmptyState, ErrorState, Spinner} from '@/components/shared'
import {relativeTime, absoluteTime} from '@/lib/time'
import {useAsync} from '@/hooks/useAsync'

export const HistoryTab: React.FC = () => {
  const document = useFlowStore(s => s.document)
  const [isSaving, setIsSaving] = useState(false)
  const [comment, setComment] = useState('')
  const [saveError, setSaveError] = useState<string | null>(null)

  const {
    data,
    isLoading,
    error: fetchError,
    refetch: fetchVersions,
  } = useAsync<FlowVersion[]>(
    () => (document?.id ? versionsApi.list(document.id, 50) : Promise.resolve([])),
    [document?.id],
  )
  // Clear stale results on a fetch error (matches the previous behavior of
  // resetting to [] on catch) rather than showing a stale list under an error.
  const versions = fetchError ? [] : (data ?? [])
  const error = saveError ?? (fetchError ? 'Failed to load versions' : null)

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
      setSaveError(err instanceof Error ? err.message : 'Failed to save version')
    } finally {
      setIsSaving(false)
    }
  }

  if (!document) {
    return (
      <div className="p-8 text-center text-text-tertiary">
        <History size={32} className="mx-auto mb-2 opacity-20" />
        <p className="text-sm">Open a flow to view version history.</p>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full bg-surface-1 overflow-y-auto">
      <div className="p-4 border-b border-border-subtle bg-surface-2/50">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3 flex items-center gap-1.5">
          <Plus size={13} />
          Save Snapshot
        </h3>
        <form onSubmit={handleSave} className="flex gap-2">
          <input
            type="text"
            placeholder="Optional comment…"
            value={comment}
            onChange={e => setComment(e.target.value)}
            className="flex-1 bg-surface-0 border border-border-default rounded-md px-3 py-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-brand-500/50"
          />
          <button
            type="submit"
            disabled={isSaving}
            className="px-3 py-1.5 bg-brand-600 text-brand-foreground rounded-md text-xs font-semibold hover:bg-brand-700 disabled:opacity-50 transition-colors"
          >
            {isSaving ? 'Saving…' : 'Save'}
          </button>
        </form>
        {error && <p className="text-2xs text-red-500 mt-1">{error}</p>}
      </div>

      <div className="flex-1 p-4">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3">Version History</h3>
        {isLoading ? (
          <div className="flex justify-center p-8">
            <Spinner size={20} />
          </div>
        ) : versions.length === 0 ? (
          error ? (
            <ErrorState message={error} onRetry={fetchVersions} />
          ) : (
            <EmptyState title="No history yet" description="Run analysis to create snapshots over time." />
          )
        ) : (
          <div className="space-y-2">
            {versions.map(v => (
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
                    {v.comment || <span className="text-text-tertiary italic">No comment</span>}
                  </p>
                  <p className="text-2xs text-text-tertiary mt-0.5">
                    <span title={absoluteTime(v.createdAt)}>{relativeTime(v.createdAt)}</span>
                    {v.metadata?.blockCount != null && (
                      <span className="ml-2 text-text-muted">{v.metadata.blockCount} blocks</span>
                    )}
                  </p>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
