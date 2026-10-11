import type {Dispatch, ReactNode, SetStateAction} from 'react'
import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Plus} from 'lucide-react'
import {Button} from '@/components/shared'
import type {Organisation} from '@/stores/orgStore'

interface OrgCreateSectionProps {
  banner: ReactNode
  createOrg: (name: string) => Promise<Organisation>
  setActiveOrg: (id: string | null) => void
  setForbidden: Dispatch<SetStateAction<boolean>>
  reportFailure: (err: unknown) => void
}

export default function OrgCreateSection({
  banner,
  createOrg,
  setActiveOrg,
  setForbidden,
  reportFailure,
}: OrgCreateSectionProps) {
  const {t} = useTranslation('settings')
  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [createBusy, setCreateBusy] = useState(false)

  const handleCreate = async () => {
    const name = newName.trim()
    if (!name) return
    setCreateBusy(true)
    setForbidden(false)
    try {
      const o = await createOrg(name)
      setActiveOrg(o.id)
      setNewName('')
      setCreating(false)
    } catch (err) {
      reportFailure(err)
    } finally {
      setCreateBusy(false)
    }
  }

  return (
    <>
      {!creating && (
        <div className="flex justify-end">
          <Button variant="primary" icon={Plus} onClick={() => setCreating(true)}>
            {t('orgs.newOrg')}
          </Button>
        </div>
      )}
      {banner}
      {creating && (
        <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-3">
          <label htmlFor="new-org-name" className="block text-sm font-medium text-text-primary">
            {t('orgs.nameLabel')}
          </label>
          <input
            id="new-org-name"
            autoFocus
            value={newName}
            onChange={e => setNewName(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') void handleCreate()
            }}
            placeholder={t('orgs.namePlaceholder')}
            className="w-full px-3 py-2 rounded-lg bg-surface-2 border border-border-default text-sm focus:outline-hidden focus:border-brand-500"
          />
          <div className="flex gap-2 justify-end">
            <button
              onClick={() => {
                setCreating(false)
                setNewName('')
              }}
              className="px-3 py-1.5 rounded-lg text-sm text-text-secondary hover:bg-surface-3"
            >
              {t('orgs.cancel')}
            </button>
            <button
              onClick={handleCreate}
              disabled={!newName.trim() || createBusy}
              className="px-3 py-1.5 rounded-lg bg-brand-500 hover:bg-brand-600 disabled:opacity-50 text-brand-foreground text-sm font-medium"
            >
              {createBusy ? t('orgs.creating') : t('orgs.create')}
            </button>
          </div>
        </div>
      )}
    </>
  )
}
