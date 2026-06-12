import { useEffect, useState } from 'react'
import { UserPlus } from 'lucide-react'
import Modal from '@/components/shared/Modal'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import Spinner from '@/components/shared/Spinner'
import CollaboratorList from './CollaboratorList'
import PermissionSelect from './PermissionSelect'
import { sharingApi, type Collaborator, type Permission } from '@/api/sharing'
import { useAuthStore } from '@/stores/authStore'

interface ShareDialogProps {
  flowId: string
  flowName: string
  open: boolean
  onClose: () => void
}

export default function ShareDialog({ flowId, flowName, open, onClose }: ShareDialogProps) {
  const currentUserId = useAuthStore(s => s.user?.id)
  const [collaborators, setCollaborators] = useState<Collaborator[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [inviteEmail, setInviteEmail] = useState('')
  const [invitePermission, setInvitePermission] = useState<Permission>('viewer')
  const [isInviting, setIsInviting] = useState(false)

  useEffect(() => {
    if (!open) return
    setIsLoading(true)
    sharingApi.listCollaborators(flowId)
      .then(setCollaborators)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setIsLoading(false))
  }, [flowId, open])

  async function handleInvite(e: React.FormEvent) {
    e.preventDefault()
    if (!inviteEmail.trim()) return
    setIsInviting(true)
    setError(null)
    try {
      // For now send userId as email — the backend resolves the user
      const added = await sharingApi.addCollaborator({
        flowId,
        userId: inviteEmail.trim(),
        permission: invitePermission,
      })
      setCollaborators(prev => [...prev, added])
      setInviteEmail('')
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setIsInviting(false)
    }
  }

  async function handleChangePermission(userId: string, permission: Permission) {
    try {
      const updated = await sharingApi.updatePermission({ flowId, userId, permission })
      setCollaborators(prev => prev.map(c => c.userId === userId ? updated : c))
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  async function handleRemove(userId: string) {
    try {
      await sharingApi.removeCollaborator(flowId, userId)
      setCollaborators(prev => prev.filter(c => c.userId !== userId))
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <Modal isOpen={open} onClose={onClose} title={`Share "${flowName}"`}>
      <div className="flex flex-col gap-4 min-w-[400px]">
        {error && (
          <div className="px-3 py-2 text-sm bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-semantic-error">
            {error}
          </div>
        )}

        <form onSubmit={handleInvite} className="flex gap-2">
          <Input
            type="email"
            placeholder="Email address"
            value={inviteEmail}
            onChange={e => setInviteEmail(e.target.value)}
            className="flex-1"
          />
          <PermissionSelect value={invitePermission} onChange={setInvitePermission} />
          <Button
            type="submit"
            variant="primary"
            icon={UserPlus}
            loading={isInviting}
            disabled={isInviting || !inviteEmail.trim()}
          >
            Invite
          </Button>
        </form>

        <div className="border-t border-border-default pt-3">
          <p className="text-xs font-medium text-text-muted uppercase tracking-wide mb-2">
            People with access
          </p>
          {isLoading
            ? <div className="flex justify-center py-4"><Spinner /></div>
            : (
              <CollaboratorList
                collaborators={collaborators}
                currentUserId={currentUserId}
                onChangePermission={handleChangePermission}
                onRemove={handleRemove}
              />
            )
          }
        </div>
      </div>
    </Modal>
  )
}
