import React from 'react'
import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'
import {roleBadgeClass} from '@/lib/roleBadge'

export function SectionCard({
  id,
  icon: Icon,
  title,
  description,
  action,
  tone = 'default',
  className,
  children,
}: {
  id: string
  icon: LucideIcon
  title: string
  description?: string
  action?: React.ReactNode
  tone?: 'default' | 'danger'
  className?: string
  children?: React.ReactNode
}) {
  const headingId = `${id}-heading`
  const danger = tone === 'danger'
  return (
    <section
      id={id}
      aria-labelledby={headingId}
      className={clsx(
        'profile-card rounded-xl border overflow-hidden',
        danger ? 'border-semantic-error/40 bg-surface-2' : 'border-border-default bg-surface-2',
        className,
      )}
    >
      <div
        className={clsx(
          'profile-card-head flex items-start gap-3 px-5 py-4',
          children && 'border-b',
          danger ? 'border-semantic-error/20' : 'border-border-subtle',
        )}
      >
        <span
          className={clsx(
            'w-8 h-8 rounded-lg flex items-center justify-center shrink-0',
            danger ? 'bg-semantic-error/10 text-semantic-error' : 'bg-surface-3 text-text-secondary',
          )}
          aria-hidden="true"
        >
          <Icon size={15} />
        </span>
        <div className="profile-card-title pt-0.5">
          <h3
            id={headingId}
            tabIndex={-1}
            className={clsx('text-sm font-semibold outline-hidden', danger ? 'text-semantic-error' : 'text-text-primary')}
          >
            {title}
          </h3>
          {description && <p className="text-xs text-text-tertiary mt-0.5">{description}</p>}
        </div>
        {action && <div className="profile-card-action shrink-0 flex items-center gap-2">{action}</div>}
      </div>
      {children && <div className="px-5 py-4">{children}</div>}
    </section>
  )
}

export function SectionNotice({message, action}: {message: string; action?: React.ReactNode}) {
  return (
    <div
      role="alert"
      className="flex items-center justify-between gap-3 rounded-lg px-3 py-2 mb-3 text-sm border bg-semantic-error/10 border-semantic-error/30 text-semantic-error"
    >
      <span className="min-w-0">{message}</span>
      {action && <span className="shrink-0">{action}</span>}
    </div>
  )
}

export function RoleBadge({role, className}: {role: string; className?: string}) {
  return (
    <span
      className={clsx(
        'inline-block px-2 py-0.5 rounded-md text-2xs font-semibold uppercase tracking-wide',
        roleBadgeClass(role),
        className,
      )}
    >
      {role}
    </span>
  )
}
