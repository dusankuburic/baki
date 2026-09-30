import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'

type EmptyStateProps = {
  icon?: LucideIcon
  title: string
  description?: string
  // The heading level that fits the outline it sits in: an h3 under a
  // section's h2 by default, deeper inside a card (h4/h5).
  headingLevel?: 2 | 3 | 4 | 5 | 6
  // Extra content between the description and the action: a short list of
  // what the user needs, a secondary link.
  children?: React.ReactNode
  action?: React.ReactNode
  className?: string
}

export default function EmptyState({
  icon: Icon,
  title,
  description,
  headingLevel = 3,
  children,
  action,
  className,
}: EmptyStateProps) {
  const Heading = `h${headingLevel}` as const
  return (
    <div className={clsx('flex flex-col items-center justify-center py-12 px-4', className)}>
      {Icon && (
        <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center mb-4">
          <Icon size={24} className="text-text-tertiary" aria-hidden="true" />
        </div>
      )}
      <Heading className="text-base font-medium text-text-primary mb-1">{title}</Heading>
      {description && <p className="text-sm text-text-tertiary text-center max-w-xs">{description}</p>}
      {children && <div className="mt-3">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  )
}
