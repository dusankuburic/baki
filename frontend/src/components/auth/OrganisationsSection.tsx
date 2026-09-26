import {useTranslation} from 'react-i18next'
import React from 'react'
import {Building2, Settings2} from 'lucide-react'
import {type Organisation} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {Button, Skeleton} from '@/components/shared'
import {SectionCard, RoleBadge} from './SectionCard'

export const OrganisationsSection: React.FC<{
  organisations: Organisation[]
  userId: string
  loading?: boolean
}> = ({organisations, userId, loading = false}) => {
  const {t} = useTranslation('auth')
  const openSettings = useUIStore(s => s.openSettings)
  // Creating, renaming and inviting live in Settings → Organisations; the
  // profile only reads memberships, so it links there instead of duplicating.
  const openOrgSettings = () => openSettings('orgs')

  return (
    <SectionCard
      id="profile-organisations"
      icon={Building2}
      title={t('organisations.heading')}
      description={t('organisations.description')}
      action={
        <Button variant="ghost" size="sm" icon={Settings2} onClick={openOrgSettings}>
          {t('organisations.manage')}
        </Button>
      }
    >
      {loading && organisations.length === 0 ? (
        <Skeleton lines={2} />
      ) : organisations.length === 0 ? (
        <div>
          <p className="text-sm text-text-secondary">{t('organisations.empty')}</p>
          <p className="text-xs text-text-tertiary mt-1">{t('organisations.emptyHint')}</p>
        </div>
      ) : (
        <ul className="divide-y divide-border-subtle">
          {organisations.map(org => {
            const membership = org.members.find(m => m.userId === userId)
            return (
              <li key={org.id} className="flex items-center gap-3 py-2.5 first:pt-0 last:pb-0">
                <span
                  className="w-9 h-9 rounded-lg bg-surface-3 text-text-secondary flex items-center justify-center shrink-0"
                  aria-hidden="true"
                >
                  <Building2 size={16} />
                </span>
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-text-primary truncate">{org.name}</p>
                  <p className="text-xs text-text-tertiary">
                    {t('organisations.members', {count: org.members.length})}
                  </p>
                </div>
                {membership && <RoleBadge role={membership.role} className="shrink-0" />}
              </li>
            )
          })}
        </ul>
      )}
    </SectionCard>
  )
}
