import type {ReactNode} from 'react'

export function ChartSection({
  id,
  title,
  description,
  children,
}: {
  id: string
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <section id={id} aria-labelledby={`${id}-heading`} className="analytics-section">
      <div className="mb-3">
        <h2 id={`${id}-heading`} tabIndex={-1} className="text-base font-semibold text-text-primary outline-hidden">
          {title}
        </h2>
        {description && <p className="mt-0.5 text-xs text-text-tertiary">{description}</p>}
      </div>
      <div className="analytics-grid">{children}</div>
    </section>
  )
}
