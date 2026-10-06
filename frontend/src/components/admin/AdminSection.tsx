import type {ReactNode} from 'react'

// One section of the Admin Dashboard: the h2 the section nav lands focus on
// (useActiveSection.jumpTo focuses the section's h2), a one-line description,
// an optional action slot, and whatever the section renders. Cards inside a
// section use SectionCard, whose heading is an h3.
export function AdminSection({
  id,
  title,
  description,
  action,
  children,
}: {
  id: string
  title: string
  description?: string
  action?: ReactNode
  children: ReactNode
}) {
  return (
    <section id={id} aria-labelledby={`${id}-heading`} className="admin-section">
      <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 id={`${id}-heading`} tabIndex={-1} className="text-base font-semibold text-text-primary outline-hidden">
            {title}
          </h2>
          {description && <p className="mt-0.5 text-xs text-text-tertiary">{description}</p>}
        </div>
        {action && <div className="flex shrink-0 items-center gap-2">{action}</div>}
      </div>
      {children}
    </section>
  )
}

// Placeholder rows while a table loads its first page: the same row height
// as the real rows, so the section does not jump when they land.
export function SkeletonRows({rows, cols}: {rows: number; cols: number}) {
  return (
    <>
      {Array.from({length: rows}, (_, r) => (
        <tr key={r} aria-hidden="true">
          {Array.from({length: cols}, (_, c) => (
            <td key={c} className="px-4 py-3">
              <div className="h-3.5 rounded bg-surface-3 animate-pulse-soft" style={{width: c === 0 ? '60%' : '40%'}} />
            </td>
          ))}
        </tr>
      ))}
    </>
  )
}
