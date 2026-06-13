import React from 'react'
import {Building2} from 'lucide-react'
import clsx from 'clsx'
import {type Organisation} from '@/stores/orgStore'

function roleBadgeClass(role: string) {
  switch (role) {
    case 'admin':   return 'bg-block-subflow/10 text-block-subflow'
    case 'member':  return 'bg-block-action/10 text-block-action'
    case 'viewer':  return 'bg-block-condition/10 text-block-condition'
    default:        return 'bg-surface-4 text-text-tertiary'
  }
}

export const OrganisationsCard: React.FC<{
  organisations: Organisation[]
  userId: string
}> = ({organisations, userId}) => (
  <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
    <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
      <Building2 size={13} className="text-text-tertiary" />
      <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Organisations</h2>
    </div>

    <div className="p-5">
      {organisations.length === 0 ? (
        <p className="text-sm text-text-tertiary">You're not a member of any organisations.</p>
      ) : (
        <ul className="space-y-2">
          {organisations.map(org => {
            const membership = org.members.find(m => m.userId === userId)
            return (
              <li key={org.id} className="flex items-center justify-between gap-3 rounded-lg bg-surface-3 px-3 py-2.5">
                <span className="text-sm text-text-primary truncate">{org.name}</span>
                {membership && (
                  <span className={clsx(
                    'px-2 py-0.5 rounded-md text-xs font-semibold uppercase flex-shrink-0',
                    roleBadgeClass(membership.role)
                  )}>
                    {membership.role}
                  </span>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </div>
  </div>
)
