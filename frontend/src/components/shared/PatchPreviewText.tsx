import clsx from 'clsx'
import {useMemo} from 'react'

type Seg = {kind: 'header' | 'added' | 'removed' | 'context'; text: string}

function classify(text: string): Seg[] {
  const out: Seg[] = []
  for (const raw of text.split('\n')) {
    if (/^\s*\+\s/.test(raw)) {
      out.push({kind: 'added', text: raw.replace(/^\s*\+\s?/, '')})
    } else if (/\bremove lines?\b/i.test(raw)) {
      out.push({kind: 'removed', text: raw.trim()})
    } else if (/^Fix .* for rule /.test(raw.trim())) {
      out.push({kind: 'header', text: raw.trim()})
    } else if (/^\s{2,}(insert|wrap|append|on line|remove)/.test(raw)) {
      out.push({kind: 'header', text: raw.trim()})
    } else {
      out.push({kind: 'context', text: raw})
    }
  }
  return out
}

export default function PatchPreviewText({text, className}: {text: string; className?: string}) {
  const segs = useMemo(() => classify(text), [text])
  return (
    <div
      className={clsx(
        'overflow-auto whitespace-pre-wrap rounded bg-surface-2 p-1.5 text-2xs leading-relaxed',
        className,
      )}
      data-testid="patch-preview"
    >
      {segs.map((seg, i) => (
        <div
          key={i}
          className={clsx(
            'flex items-start gap-1.5 rounded-sm',
            seg.kind === 'added' && 'bg-semantic-success/10 px-1 text-semantic-success',
            seg.kind === 'removed' && 'bg-semantic-error/10 px-1 text-semantic-error',
            seg.kind === 'header' && 'px-1 font-medium text-text-tertiary',
            seg.kind === 'context' && 'px-1 text-text-secondary',
          )}
        >
          {seg.kind === 'added' ? (
            <span className="select-none font-bold text-semantic-success">+</span>
          ) : seg.kind === 'removed' ? (
            <span className="select-none font-bold text-semantic-error">−</span>
          ) : (
            <span className="select-none w-2 shrink-0" aria-hidden="true" />
          )}
          <span className="min-w-0 flex-1 break-all font-mono">{seg.text}</span>
        </div>
      ))}
    </div>
  )
}
