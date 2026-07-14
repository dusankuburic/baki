import clsx from 'clsx'
import type {Permission} from '@/api/sharing'

const LABELS: Record<Permission, string> = {
  viewer: 'Viewer',
  editor: 'Editor',
  admin: 'Admin',
}

const DESCRIPTIONS: Record<Permission, string> = {
  viewer: 'Can view',
  editor: 'Can edit',
  admin: 'Full control',
}

interface PermissionSelectProps {
  value: Permission
  onChange: (p: Permission) => void
  disabled?: boolean
  className?: string
}

export default function PermissionSelect({value, onChange, disabled, className}: PermissionSelectProps) {
  return (
    <select
      value={value}
      onChange={e => onChange(e.target.value as Permission)}
      disabled={disabled}
      aria-label="Permission level"
      className={clsx(
        'h-8 px-2 pr-7 text-sm rounded-md border border-border-default',
        'bg-surface-2 text-text-primary',
        'focus:outline-none focus:ring-2 focus:ring-brand-500/50',
        'disabled:opacity-50 disabled:cursor-not-allowed',
        className,
      )}
    >
      {(Object.keys(LABELS) as Permission[]).map(p => (
        <option key={p} value={p} title={DESCRIPTIONS[p]}>
          {LABELS[p]}
        </option>
      ))}
    </select>
  )
}
