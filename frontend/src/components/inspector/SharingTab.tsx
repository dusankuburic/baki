import React, { useState, useEffect } from 'react'
import { Users, UserPlus, Trash2, Shield, Eye, Edit3 } from 'lucide-react'
import { sharingApi, type Collaborator, type Permission } from '@/api/sharing'
import { useFlowStore } from '@/stores/flowStore'
import { useAuthStore } from '@/stores/authStore'
import { Spinner } from '@/components/shared'

export const SharingTab: React.FC = () => {
  const document = useFlowStore(s => s.document)
  const currentUser = useAuthStore(s => s.user)
  const [collaborators, setCollaborators] = useState<Collaborator[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [newEmail, setNewEmail] = useState('')
  const [newPermission, setNewPermission] = useState<Permission>('viewer')
  const [isAdding, setIsAdding] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const fetchCollaborators = async () => {
    if (!document) return
    setIsLoading(true)
    setError(null)
    try {
      const list = await sharingApi.listCollaborators(document.id)
      setCollaborators(list)
    } catch (err) {
      console.error('Failed to fetch collaborators', err)
      setError('Failed to load collaborators')
    } finally {
      setIsLoading(false)
    }
  }

  useEffect(() => {
    fetchCollaborators()
  }, [document?.id])

  const handleAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!document || !newEmail) return
    
    setIsAdding(true)
    setError(null)
    try {
      await sharingApi.addCollaborator({
        flowId: document.id,
        email: newEmail,
        permission: newPermission
      })
      setNewEmail('')
      fetchCollaborators()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to add collaborator')
    } finally {
      setIsAdding(false)
    }
  }

  const handleRemove = async (userId: string) => {
    if (!document) return
    if (!confirm('Remove this collaborator?')) return
    
    try {
      await sharingApi.removeCollaborator(document.id, userId)
      fetchCollaborators()
    } catch (err) {
      alert('Failed to remove: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleUpdatePermission = async (userId: string, perm: Permission) => {
    if (!document) return
    try {
      await sharingApi.updatePermission({
        flowId: document.id,
        userId,
        permission: perm
      })
      fetchCollaborators()
    } catch (err) {
      alert('Failed to update: ' + (err instanceof Error ? err.message : 'Unknown error'))
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
              className="px-4 py-1 bg-brand-600 text-white rounded-md text-xs font-semibold hover:bg-brand-700 disabled:opacity-50 transition-colors shadow-sm"
            >
              {isAdding ? 'Inviting...' : 'Invite'}
            </button>
          </div>
          {error && <p className="text-[10px] text-red-500 font-medium">{error}</p>}
        </form>
      </div>

      <div className="flex-1 p-4">
        <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-3">
          Current Collaborators
        </h3>
        {isLoading ? (
          <div className="flex justify-center p-8"><Spinner size={20} /></div>
        ) : collaborators.length > 0 ? (
          <div className="space-y-3">
            {collaborators.map(c => (
              <div key={c.userId} className="flex items-center justify-between p-2 rounded-lg bg-surface-2 border border-border-subtle/50 group">
                <div className="flex flex-col min-w-0">
                  <span className="text-xs font-medium text-text-primary truncate">{c.email}</span>
                  <span className="text-[10px] text-text-tertiary flex items-center gap-1">
                    {c.permission === 'admin' ? <Shield size={10} /> : 
                     c.permission === 'editor' ? <Edit3 size={10} /> : <Eye size={10} />}
                    <span className="capitalize">{c.permission}</span>
                  </span>
                </div>
                <div className="flex items-center gap-1">
                   {c.userId !== currentUser?.id && (
                     <select
                        value={c.permission}
                        onChange={(e) => handleUpdatePermission(c.userId, e.target.value as Permission)}
                        className="bg-transparent border-none text-[10px] text-text-tertiary hover:text-text-primary focus:ring-0 cursor-pointer"
                      >
                        <option value="viewer">Viewer</option>
                        <option value="editor">Editor</option>
                        <option value="admin">Admin</option>
                      </select>
                   )}
                   {c.userId !== currentUser?.id && (
                    <button
                      onClick={() => handleRemove(c.userId)}
                      className="p-1.5 text-text-tertiary hover:text-red-500 rounded-md opacity-0 group-hover:opacity-100 transition-opacity"
                      title="Remove access"
                    >
                      <Trash2 size={14} />
                    </button>
                   )}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <div className="text-center py-8 text-text-muted text-xs">
            No collaborators yet.
          </div>
        )}
      </div>
    </div>
  )
}
