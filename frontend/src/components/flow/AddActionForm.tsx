import {useMemo, useState} from 'react'
import {Plus, X} from 'lucide-react'
import {Button} from '@/components/shared'

// composeActionLine builds one PAD action line from the form's parts — the
// structured-editing slice (R3-1): users who don't know PAD's text format can
// still build flows. Quoting stays the user's choice (the raw-parameters
// field passes through verbatim, matching what the source editor accepts).
export function composeActionLine(rawType: string, params: string, output: string): string {
  let line = rawType.trim()
  const p = params.trim()
  if (p) line += ' ' + p
  const o = output.trim()
  if (o) line += ' => ' + o
  return line
}

// insertBeforeLastRegionEnd inserts line into source before the FINAL
// `#EndRegion` marker (4-space indent, the region-body convention), so the
// new action lands at the end of the last subflow. Sources without a region
// end marker append at the end. Returns the new source.
export function insertBeforeLastRegionEnd(source: string, line: string): string {
  const lines = source.split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].trim().toLowerCase() === '#endregion') {
      const out = [...lines]
      out.splice(i, 0, '    ' + line)
      return out.join('\n')
    }
  }
  const sep = source.endsWith('\n') ? '' : '\n'
  return source + sep + '    ' + line + '\n'
}

interface Props {
  onInsert: (line: string) => void
  onClose: () => void
}

// AddActionForm is the structured "add block" editor: action type + raw
// parameters + output variable → one composed PAD line, previewed live
// before insertion.
export default function AddActionForm({onInsert, onClose}: Props) {
  const [rawType, setRawType] = useState('')
  const [params, setParams] = useState('')
  const [output, setOutput] = useState('')

  const preview = useMemo(() => composeActionLine(rawType, params, output), [rawType, params, output])
  const valid = rawType.trim().length > 0 && /^[A-Za-z][A-Za-z0-9_.]*$/.test(rawType.trim())

  return (
    <div
      className="mx-3 my-2 rounded-lg border border-brand-500/30 bg-brand-500/5 p-3"
      data-testid="add-action-form"
      role="dialog"
      aria-label="Add action"
    >
      <div className="flex items-center justify-between mb-2">
        <span className="text-2xs font-semibold uppercase tracking-wide text-brand-300">Add action</span>
        <button
          onClick={onClose}
          className="p-1 rounded text-text-tertiary hover:text-text-secondary"
          aria-label="Close add action"
        >
          <X size={12} />
        </button>
      </div>
      <div className="space-y-2">
        <input
          value={rawType}
          onChange={e => setRawType(e.target.value)}
          placeholder="Action type — e.g. Display.ShowMessageBox"
          autoFocus
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500 font-mono"
          aria-label="Action type"
        />
        <input
          value={params}
          onChange={e => setParams(e.target.value)}
          placeholder="Parameters — e.g. Message: $'''Hello''' Icon: Display.Icon.Information"
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500 font-mono"
          aria-label="Parameters"
        />
        <input
          value={output}
          onChange={e => setOutput(e.target.value)}
          placeholder="Output variable (optional) — e.g. ButtonPressed"
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500 font-mono"
          aria-label="Output variable"
        />
        {preview && (
          <div className="rounded bg-surface-2 border border-border-subtle px-2 py-1.5">
            <div className="text-2xs text-text-tertiary mb-0.5">Preview</div>
            <code className="text-2xs text-brand-300 font-mono break-all">{preview}</code>
          </div>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            disabled={!valid}
            icon={Plus}
            onClick={() => {
              onInsert(preview)
            }}
          >
            Insert at end
          </Button>
        </div>
        {!valid && rawType.trim().length > 0 && (
          <p className="text-2xs text-semantic-warning">
            Action type must look like Module.Action (letters, digits, dots).
          </p>
        )}
      </div>
    </div>
  )
}
