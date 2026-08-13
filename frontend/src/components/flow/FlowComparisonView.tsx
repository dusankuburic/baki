import {useState, useMemo} from 'react'
import {GitCompare, ArrowRight, Plus, Minus, RefreshCw, Check} from 'lucide-react'
import {libraryApi, type LibraryFlow} from '@/api/library'
import {analysisApi} from '@/api'
import {Spinner, EmptyState} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {logger} from '@/lib/logger'
import type {FlowComparison, SubflowComparison} from '@/types'

// changeAppearance maps a block-comparison change kind to the icon + color used
// in the diff renderer. The backend's CompareFlows stamps `change` as a short
// token ("same"/"added"/"removed"/"modified"); anything unrecognized reads as
// "modified" so a new token never renders blank.
function changeAppearance(change: string): {Icon: typeof Plus; color: string; label: string} {
  const c = change.toLowerCase()
  if (c === 'added' || c === 'new') return {Icon: Plus, color: 'text-semantic-success', label: 'Added'}
  if (c === 'removed' || c === 'deleted') return {Icon: Minus, color: 'text-semantic-error', label: 'Removed'}
  if (c === 'same' || c === 'unchanged') return {Icon: Check, color: 'text-text-tertiary', label: 'Same'}
  return {Icon: RefreshCw, color: 'text-semantic-warning', label: 'Modified'}
}

function similarityPct(s?: number): string {
  if (s == null) return '—'
  return `${Math.round(s * 100)}%`
}

// FlowPicker is a labelled <select> over the user's library flows, with the
// "compare against" flow filtered out of the second picker so the same flow
// can't be picked twice.
function FlowPicker({
  label,
  value,
  onChange,
  flows,
  exclude,
}: {
  label: string
  value: string
  onChange: (id: string) => void
  flows: LibraryFlow[]
  exclude?: string
}) {
  return (
    <label className="flex flex-col gap-1.5 flex-1 min-w-0">
      <span className="text-2xs uppercase tracking-wider text-text-tertiary">{label}</span>
      <select
        value={value}
        onChange={e => onChange(e.target.value)}
        className="px-3 py-2 rounded-lg border border-border-default bg-surface-2 text-sm text-text-primary outline-none focus:border-brand-500"
      >
        <option value="">Select a flow…</option>
        {flows
          .filter(f => f.id !== exclude)
          .map(f => (
            <option key={f.id} value={f.id}>
              {f.name}
            </option>
          ))}
      </select>
    </label>
  )
}

function SubflowRow({sf}: {sf: SubflowComparison}) {
  const [open, setOpen] = useState(false)
  const name = sf.subflowA || sf.subflowB || '(unnamed)'
  return (
    <div className="border border-border-subtle rounded-lg bg-surface-1">
      <button
        onClick={() => setOpen(o => !o)}
        className="w-full flex items-center gap-3 px-4 py-2.5 text-left hover:bg-surface-2/50 transition-colors"
      >
        <span className="text-sm font-medium text-text-primary truncate flex-1">{name}</span>
        <span className="text-2xs text-text-tertiary tabular-nums">
          {sf.blockDiffs.length} block{sf.blockDiffs.length !== 1 ? 's' : ''} · {similarityPct(sf.similarity)}
        </span>
      </button>
      {open && (
        <div className="border-t border-border-subtle divide-y divide-border-subtle">
          {sf.blockDiffs.map((b, i) => {
            const app = changeAppearance(b.change)
            const Icon = app.Icon
            return (
              <div key={i} className="flex items-center gap-3 px-4 py-1.5">
                <Icon size={12} className={`${app.color} shrink-0`} />
                <span className={`text-2xs font-medium uppercase tracking-wider ${app.color} w-16 shrink-0`}>
                  {app.label}
                </span>
                <span className="text-xs text-text-primary truncate flex-1">
                  {b.blockA?.name ?? b.blockB?.name ?? '(block)'}
                </span>
                {b.similarity != null && b.change !== 'same' && (
                  <span className="text-2xs text-text-tertiary tabular-nums">{similarityPct(b.similarity)}</span>
                )}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

/**
 * Flow-vs-flow comparison: pick two library flows and render the similarity-
 * scored diff (per-subflow block adds/removes/modifications + a headline
 * similarity). The backend compare endpoint (analysisApi.compareFlows) existed
 * but had no UI; this wires it up. Reached via the command palette
 * ("Compare Two Flows") in cloud mode.
 */
export default function FlowComparisonView() {
  const [flowA, setFlowA] = useState('')
  const [flowB, setFlowB] = useState('')
  const [result, setResult] = useState<FlowComparison | null>(null)

  // Load the user's library flows once for the pickers.
  const {data: page} = useAsync(() => libraryApi.list({limit: 200}), [])
  const flows = useMemo(() => page?.items ?? [], [page])

  const [comparing, setComparing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const compare = async () => {
    if (!flowA || !flowB) return
    setComparing(true)
    setError(null)
    try {
      const res = await analysisApi.compareFlows(flowA, flowB)
      setResult(res)
    } catch (e) {
      setError(String(e))
      logger.warn('Flow comparison failed', e)
    } finally {
      setComparing(false)
    }
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-4xl mx-auto p-6 space-y-6">
        <div>
          <h1 className="text-xl font-semibold text-text-primary flex items-center gap-2">
            <GitCompare size={20} className="text-brand-500" />
            Compare Flows
          </h1>
          <p className="text-sm text-text-secondary mt-1">
            Side-by-side similarity diff of two library flows — block adds, removals, and modifications per subflow.
          </p>
        </div>

        <div className="flex items-end gap-3 p-4 border border-border-default rounded-xl bg-surface-1">
          <FlowPicker label="Flow A" value={flowA} onChange={setFlowA} flows={flows} exclude={flowB} />
          <ArrowRight size={16} className="text-text-tertiary shrink-0 mb-2.5" />
          <FlowPicker label="Flow B" value={flowB} onChange={setFlowB} flows={flows} exclude={flowA} />
          <button
            onClick={compare}
            disabled={!flowA || !flowB || comparing}
            className="flex items-center gap-1.5 px-4 py-2 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors disabled:opacity-50 shrink-0"
          >
            {comparing ? <Spinner /> : <GitCompare size={14} />}
            Compare
          </button>
        </div>

        {error && <p className="text-sm text-semantic-error">{error}</p>}

        {result && (
          <>
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
              <Stat label="Similarity" value={similarityPct(result.similarity)} />
              <Stat label="Shared blocks" value={result.sharedBlocks} />
              <Stat label="Added" value={result.addedBlocks} accent="text-semantic-success" />
              <Stat label="Removed" value={result.removedBlocks} accent="text-semantic-error" />
            </div>

            <div className="space-y-2">
              {result.subflowDiff.length === 0 ? (
                <EmptyState title="No subflow differences" description="The two flows have no comparable subflow structure." />
              ) : (
                result.subflowDiff.map((sf, i) => <SubflowRow key={i} sf={sf} />)
              )}
            </div>
          </>
        )}
      </div>
    </div>
  )
}

function Stat({label, value, accent}: {label: string; value: string | number; accent?: string}) {
  return (
    <div className="px-4 py-3 rounded-xl border border-border-default bg-surface-1">
      <div className={`text-2xl font-semibold tabular-nums ${accent ?? 'text-text-primary'}`}>{value}</div>
      <div className="text-2xs uppercase tracking-wider text-text-tertiary mt-0.5">{label}</div>
    </div>
  )
}
