import {useState, useMemo, useEffect} from 'react'
import clsx from 'clsx'
import {Modal, Button} from '@/components/shared'

interface DiffLine {
  type: 'added' | 'removed' | 'context'
  text: string
}

// computeDiff produces a simple line-by-line diff between two texts. Uses a
// basic LCS algorithm — adequate for small patches (typically <20 lines changed).
// To avoid O(n*m) blowup on large files, both inputs are pre-trimmed to a
// window around the first/last changed line before the LCS runs.
export function computeDiff(original: string, patched: string): DiffLine[] {
  const aFull = original.split('\n')
  const bFull = patched.split('\n')

  // Quick path: identical
  if (original === patched) return []

  // Find the window of changed lines to avoid running LCS on the full file.
  // Most patches change <20 lines; trimming to ±3 context lines around the
  // change bounds the DP table to a manageable size even for 10000-line flows.
  const maxLen = Math.max(aFull.length, bFull.length)
  let firstDiff = 0
  while (firstDiff < maxLen && aFull[firstDiff] === bFull[firstDiff]) firstDiff++
  let lastDiffA = aFull.length - 1, lastDiffB = bFull.length - 1
  while (lastDiffA >= firstDiff && lastDiffB >= firstDiff && aFull[lastDiffA] === bFull[lastDiffB]) {
    lastDiffA--; lastDiffB--
  }
  const windowBefore = 3
  const windowAfter = 3
  const trimStart = Math.max(0, firstDiff - windowBefore)
  const trimEndA = Math.min(aFull.length, lastDiffA + 1 + windowAfter)
  const trimEndB = Math.min(bFull.length, lastDiffB + 1 + windowAfter)

  const a = aFull.slice(trimStart, trimEndA)
  const b = bFull.slice(trimStart, trimEndB)
  const m = a.length, n = b.length

  // LCS DP table
  const dp: number[][] = Array.from({length: m + 1}, () => new Array(n + 1).fill(0))
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }

  // Backtrack to produce diff. The inputs are already pre-trimmed to the
  // changed window, so no further trimming is needed — just add ellipsis
  // markers if the source was truncated.
  const rawDiff: DiffLine[] = []
  let i = 0, j = 0
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      rawDiff.push({type: 'context', text: a[i]})
      i++; j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      rawDiff.push({type: 'removed', text: a[i]})
      i++
    } else {
      rawDiff.push({type: 'added', text: b[j]})
      j++
    }
  }
  while (i < m) { rawDiff.push({type: 'removed', text: a[i]}); i++ }
  while (j < n) { rawDiff.push({type: 'added', text: b[j]}); j++ }

  const result: DiffLine[] = []
  if (trimStart > 0) result.push({type: 'context', text: '⋯'})
  result.push(...rawDiff)
  if (trimEndA < aFull.length || trimEndB < bFull.length) result.push({type: 'context', text: '⋯'})
  return result
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

  // Reset applying state whenever the modal closes so re-opening doesn't
  // leave the buttons permanently disabled.
  useEffect(() => { if (!open) setApplying(false) }, [open])

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
