import React, {useState, useRef, useEffect} from 'react'
import {Users, UserPlus, Trash2, Shield, Eye, Edit3, Link2, Clock, Copy, Check} from 'lucide-react'
import {sharingApi, type Collaborator, type Permission} from '@/api/sharing'
import {flowApi, type ShareInfo} from '@/api/flow'
import {libraryApi, type LibraryFlow} from '@/api/library'
import {useFlowStore} from '@/stores/flowStore'
import {useAuthStore} from '@/stores/authStore'
import {EmptyState, Spinner, useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import {writeClipboard} from '@/lib/clipboard'
import {useAsync} from '@/hooks/useAsync'

// shareLinkURL builds the public viewer URL for a raw share token. The viewer
// is the unauthenticated /shared route (see main.tsx); BASE_URL keeps it
// correct when the app is served under a sub-path.
export function shareLinkURL(token: string): string {
  const base = (import.meta.env.BASE_URL || '/').replace(/\/?$/, '/')
  return `${window.location.origin}${base}shared?token=${encodeURIComponent(token)}`
}

export const SharingTab: React.FC = () => {
  const document = useFlowStore(s => s.document)
  const currentUser = useAuthStore(s => s.user)
  const [newEmail, setNewEmail] = useState('')
  const [newPermission, setNewPermission] = useState<Permission>('viewer')
  const [isAdding, setIsAdding] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [pendingRemoveId, setPendingRemoveId] = useState<string | null>(null)
  const removeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const {error: toastError} = useToast()

  // Clear the pending-remove confirm timer on unmount so setPendingRemoveId
  // can't fire on a gone component if the inspector closes mid-confirm.
  useEffect(() => {
    return () => {
      if (removeTimerRef.current) clearTimeout(removeTimerRef.current)
    }
  }, [])

  const {
    data,
    isLoading,
    error: fetchError,
    refetch: fetchCollaborators,
  } = useAsync<Collaborator[]>(() => {
    if (!document) return Promise.resolve([])
    return sharingApi.listCollaborators(document.id).catch(err => {
      logger.warn('Failed to fetch collaborators', err)
      throw err
    })
  }, [document?.id])
  // Stale collaborators intentionally remain visible on a fetch error (matches
  // the previous behavior — only the form's error banner reflects the failure).
  const collaborators = data ?? []
  const error = formError ?? (fetchError ? 'Failed to load collaborators' : null)

  // The server grants sharing rights to the flow OWNER, org admins and
  // admin-tier collaborators (handlers_sharing.go). The owner never appears in
  // the collaborator table, so deriving canManage from that list alone hid the
  // invite form and share links on every flow the user created themselves.
  // `canShare` from the library record is the authoritative answer; the
  // collaborator check remains as a fallback when the record cannot be read.
  const {data: libraryFlow} = useAsync<LibraryFlow | null>(() => {
    if (!document) return Promise.resolve(null)
    return libraryApi.get(document.id).catch(() => null)
  }, [document?.id])
  const canManage =
    libraryFlow?.canShare === true || collaborators.some(c => c.userId === currentUser?.id && c.permission === 'admin')

  const handleAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!document || !newEmail) return

    setIsAdding(true)
    setFormError(null)
    try {
      await sharingApi.addCollaborator({
        flowId: document.id,
        email: newEmail,
        permission: newPermission,
      })
      setNewEmail('')
      fetchCollaborators()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : 'Failed to add collaborator')
    } finally {
      setIsAdding(false)
    }
  }

  const requestRemove = (userId: string) => {
    if (removeTimerRef.current) clearTimeout(removeTimerRef.current)
    setPendingRemoveId(userId)
    removeTimerRef.current = setTimeout(() => setPendingRemoveId(null), 5000)
  }

  const cancelRemove = () => {
    if (removeTimerRef.current) clearTimeout(removeTimerRef.current)
    setPendingRemoveId(null)
  }

  const confirmRemove = async () => {
    if (!document || !pendingRemoveId) return
    const userId = pendingRemoveId
    cancelRemove()
    try {
      await sharingApi.removeCollaborator(document.id, userId)
      fetchCollaborators()
    } catch (err) {
      toastError('Failed to remove: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleUpdatePermission = async (userId: string, perm: Permission) => {
    if (!document) return
    try {
      await sharingApi.updatePermission({
        flowId: document.id,
        userId,
        permission: perm,
      })
      fetchCollaborators()
    } catch (err) {
      toastError('Failed to update: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  if (!document) {
    return (
      <div className="p-8 text-center text-text-tertiary">
        <Users size={32} className="mx-auto mb-2 opacity-20" />
        <p className="text-sm">Open a flow to manage sharing.</p>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full bg-surface-1 overflow-y-auto">
      {canManage && (
        <div className="p-4 border-b border-border-subtle bg-surface-2/50">
          <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3 flex items-center gap-1.5">
            <UserPlus size={14} />
            Invite Collaborator
          </h3>
          <form onSubmit={handleAdd} className="space-y-3">
            <input
              type="email"
              placeholder="User email address..."
              value={newEmail}
              onChange={e => setNewEmail(e.target.value)}
              required
              className="w-full bg-surface-0 border border-border-default rounded-md px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-brand-500/50"
            />
            <div className="flex gap-2">
              <select
                value={newPermission}
                onChange={e => setNewPermission(e.target.value as Permission)}
                className="flex-1 bg-surface-0 border border-border-default rounded-md px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-brand-500/50"
              >
                <option value="viewer">Viewer (Read Only)</option>
                <option value="editor">Editor (Read/Write)</option>
                <option value="admin">Admin (Full Control)</option>
              </select>
              <button
                type="submit"
                disabled={isAdding || !newEmail}
                className="px-4 py-1 bg-brand-600 text-brand-foreground rounded-md text-xs font-semibold hover:bg-brand-700 disabled:opacity-50 transition-colors shadow-sm"
              >
                {isAdding ? 'Inviting...' : 'Invite'}
              </button>
            </div>
            {error && <p className="text-2xs text-red-500 font-medium">{error}</p>}
          </form>
        </div>
      )}

      <div className="flex-1 p-4">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3">Current Collaborators</h3>
        {isLoading ? (
          <div className="flex justify-center p-8">
            <Spinner size={20} />
          </div>
        ) : collaborators.length > 0 ? (
          <div className="space-y-3">
            {collaborators.map(c => (
              <div
                key={c.userId}
                className="flex items-center justify-between p-2 rounded-lg bg-surface-2 border border-border-subtle/50 group"
              >
                <div className="flex flex-col min-w-0">
                  <span className="text-xs font-medium text-text-primary truncate">{c.email}</span>
                  <span className="text-2xs text-text-tertiary flex items-center gap-1">
                    {c.permission === 'admin' ? (
                      <Shield size={10} />
                    ) : c.permission === 'editor' ? (
                      <Edit3 size={10} />
                    ) : (
                      <Eye size={10} />
                    )}
                    <span className="capitalize">{c.permission}</span>
                  </span>
                </div>
                {canManage && c.userId !== currentUser?.id && (
                  <div className="flex items-center gap-1">
                    {pendingRemoveId === c.userId ? (
                      <div className="flex items-center gap-1 animate-fade-in">
                        <button
                          onClick={cancelRemove}
                          className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
                        >
                          Cancel
                        </button>
                        <button
                          onClick={confirmRemove}
                          className="text-2xs text-red-400 hover:bg-red-500/10 px-1.5 py-0.5 rounded font-medium transition-colors"
                        >
                          Remove
                        </button>
                      </div>
                    ) : (
                      <>
                        <select
                          value={c.permission}
                          onChange={e => handleUpdatePermission(c.userId, e.target.value as Permission)}
                          className="bg-transparent border-none text-2xs text-text-tertiary hover:text-text-primary focus:ring-0 cursor-pointer"
                        >
                          <option value="viewer">Viewer</option>
                          <option value="editor">Editor</option>
                          <option value="admin">Admin</option>
                        </select>
                        <button
                          onClick={() => requestRemove(c.userId)}
                          className="p-1.5 text-text-tertiary hover:text-red-500 rounded-md opacity-0 group-hover:opacity-100 transition-opacity"
                          title="Remove access"
                        >
                          <Trash2 size={14} />
                        </button>
                      </>
                    )}
                  </div>
                )}
              </div>
            ))}
          </div>
        ) : (
          <EmptyState
            title="No collaborators"
            description={
              canManage ? 'Share this flow to invite team members.' : 'This flow has no additional collaborators.'
            }
          />
        )}
      </div>

      {canManage && <ShareLinksSection flowId={document.id} />}
    </div>
  )
}

function ShareLinksSection({flowId}: {flowId: string}) {
  const {error: toastError, success: toastSuccess} = useToast()

  const {
    data: shares,
    isLoading,
    refetch,
  } = useAsync<ShareInfo[]>(() => flowApi.listShares(flowId).catch(() => []), [flowId])
  const shareList = shares ?? []
  const [revokingId, setRevokingId] = useState<string | null>(null)
  // The raw token is returned exactly once by createShare (the list endpoint
  // only ever has the hash), so the link must be surfaced here or it is lost.
  const [created, setCreated] = useState<{id: string; url: string; expiresAt?: string} | null>(null)
  const [copied, setCopied] = useState(false)

  const handleCreate = async () => {
    try {
      const res = await flowApi.createShare(flowId)
      setCreated({id: res.id, url: shareLinkURL(res.token), expiresAt: res.expiresAt})
      setCopied(false)
      toastSuccess('Share link created')
      refetch()
    } catch (err) {
      toastError('Failed to create share link: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleCopy = async () => {
    if (!created) return
    try {
      await writeClipboard(created.url)
      setCopied(true)
    } catch (err) {
      toastError('Could not copy: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleRevoke = async (tokenId: string) => {
    setRevokingId(tokenId)
    try {
      await flowApi.revokeShare(flowId, tokenId)
      toastSuccess('Share link revoked')
      refetch()
    } catch (err) {
      toastError('Failed to revoke: ' + (err instanceof Error ? err.message : 'Unknown error'))
    } finally {
      setRevokingId(null)
    }
  }

  const formatDate = (iso?: string) => {
    if (!iso) return ''
    const d = new Date(iso)
    return d.toLocaleDateString(undefined, {month: 'short', day: 'numeric', year: 'numeric'})
  }

  return (
    <div className="p-4 border-t border-border-subtle bg-surface-2/50">
      <div className="flex items-center justify-between mb-3">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary flex items-center gap-1.5">
          <Link2 size={14} />
          Public Share Links
        </h3>
        <button
          onClick={handleCreate}
          className="text-2xs text-brand-500 hover:text-brand-400 font-medium transition-colors"
        >
          + New Link
        </button>
      </div>

      {created && (
        <div
          role="status"
          className="mb-3 p-3 rounded-lg border border-semantic-warning/40 bg-semantic-warning/5 space-y-2 animate-fade-in"
        >
          <p className="text-2xs text-text-primary">
            Copy this link now — it is shown only once. Anyone with it can view the read-only report
            {created.expiresAt ? ` until ${formatDate(created.expiresAt)}` : ''}.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 px-2 py-1.5 rounded-md bg-surface-1 text-2xs text-text-primary break-all select-all">
              {created.url}
            </code>
            <button
              type="button"
              onClick={handleCopy}
              className="flex items-center gap-1 px-2 py-1 text-2xs font-medium rounded-md bg-surface-3 hover:bg-surface-2 text-text-primary transition-colors"
              title="Copy share link"
            >
              {copied ? <Check size={12} /> : <Copy size={12} />}
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
        </div>
      )}

      {isLoading ? (
        <div className="flex justify-center py-4">
          <Spinner size={16} />
        </div>
      ) : shareList.length > 0 ? (
        <div className="space-y-2">
          {shareList.map(s => {
            const expired = s.expiresAt && new Date(s.expiresAt) < new Date()
            return (
              <div
                key={s.id}
                className="flex items-center justify-between p-2 rounded-lg bg-surface-1 border border-border-subtle/50 group"
              >
                <div className="flex flex-col min-w-0">
                  <span className="text-2xs font-mono text-text-secondary truncate" title={s.id}>
                    Link {s.id.slice(0, 8)}
                  </span>
                  <span className="text-2xs text-text-tertiary flex items-center gap-1">
                    <Clock size={10} />
                    {expired ? (
                      <span className="text-red-400">Expired {formatDate(s.expiresAt)}</span>
                    ) : s.expiresAt ? (
                      <>Expires {formatDate(s.expiresAt)}</>
                    ) : (
                      <>Created {formatDate(s.createdAt)}</>
                    )}
                  </span>
                </div>
                <button
                  onClick={() => handleRevoke(s.id)}
                  disabled={revokingId === s.id}
                  className="p-1.5 text-text-tertiary hover:text-red-500 rounded-md opacity-0 group-hover:opacity-100 transition-opacity disabled:opacity-30"
                  title="Revoke share link"
                >
                  <Trash2 size={14} />
                </button>
              </div>
            )
          })}
        </div>
      ) : (
        <p className="text-2xs text-text-tertiary italic">
          No active share links. Create one to share a read-only report.
        </p>
      )}
    </div>
  )
}
