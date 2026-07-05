import {useState, useMemo} from 'react'
import clsx from 'clsx'
import {Modal, Button} from '@/components/shared'

interface DiffLine {
  type: 'added' | 'removed' | 'context'
  text: string
  oldLine?: number
  newLine?: number
}

// computeDiff produces a simple line-by-line diff between two texts. Uses a
// basic LCS algorithm — adequate for small patches (typically <20 lines changed).
export function computeDiff(original: string, patched: string): DiffLine[] {
  const a = original.split('\n')
  const b = patched.split('\n')
  const m = a.length, n = b.length

  // LCS DP table
  const dp: number[][] = Array.from({length: m + 1}, () => new Array(n + 1).fill(0))
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }

  // Backtrack to produce diff, then trim to changed region ±3 context lines
  const rawDiff: DiffLine[] = []
  let i = 0, j = 0
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      rawDiff.push({type: 'context', text: a[i], oldLine: i + 1, newLine: j + 1})
      i++; j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      rawDiff.push({type: 'removed', text: a[i], oldLine: i + 1})
      i++
    } else {
      rawDiff.push({type: 'added', text: b[j], newLine: j + 1})
      j++
    }
  }
  while (i < m) { rawDiff.push({type: 'removed', text: a[i], oldLine: i + 1}); i++ }
  while (j < n) { rawDiff.push({type: 'added', text: b[j], newLine: j + 1}); j++ }

  // Trim leading/trailing context to ±3 lines around changes
  const firstChange = rawDiff.findIndex(d => d.type !== 'context')
  if (firstChange === -1) return []
  let lastChange = firstChange
  for (let k = rawDiff.length - 1; k > firstChange; k--) {
    if (rawDiff[k].type !== 'context') { lastChange = k; break }
  }
  const start = Math.max(0, firstChange - 3)
  const end = Math.min(rawDiff.length, lastChange + 4)
  const trimmed = rawDiff.slice(start, end)
  if (start > 0) trimmed.unshift({type: 'context', text: '⋯'})
  if (end < rawDiff.length) trimmed.push({type: 'context', text: '⋯'})
  return trimmed
}

interface Props {
  open: boolean
  original: string
  patched: string
  fixType: string
  onApply: () => void
  onCancel: () => void
}

export default function PatchPreviewModal({open, original, patched, fixType, onApply, onCancel}: Props) {
  const [applying, setApplying] = useState(false)
  const diff = useMemo(() => computeDiff(original, patched), [original, patched])

  const handleApply = () => {
    setApplying(true)
    onApply()
  }

  return (
    <Modal isOpen={open} onClose={onCancel} title={`Preview: ${fixType}`} size="lg">
      <div className="space-y-3">
        <p className="text-xs text-text-tertiary">
          Review the source change below. The fix writes directly to the flow file and re-analyzes.
        </p>
        <div className="rounded-md border border-border-subtle overflow-hidden max-h-96 overflow-y-auto">
          <pre className="text-2xs font-mono leading-relaxed">
            {diff.map((line, idx) => (
              <div
                key={idx}
                className={clsx(
                  'px-3 py-0.5 flex items-start gap-2',
                  line.type === 'added' && 'bg-emerald-500/10 text-emerald-300',
                  line.type === 'removed' && 'bg-red-500/10 text-red-300',
                  line.type === 'context' && 'text-text-tertiary',
                )}
              >
                <span className="select-none w-4 shrink-0 text-center">
                  {line.type === 'added' ? '+' : line.type === 'removed' ? '-' : ' '}
                </span>
                <span className="flex-1 whitespace-pre-wrap break-all">{line.text}</span>
              </div>
            ))}
          </pre>
        </div>
        <div className="flex items-center justify-end gap-2 pt-1">
          <Button variant="ghost" onClick={onCancel} disabled={applying}>
            Cancel
          </Button>
          <Button variant="primary" onClick={handleApply} disabled={applying}>
            {applying ? 'Applying…' : 'Apply fix'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
