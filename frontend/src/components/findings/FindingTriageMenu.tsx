import {useState, useRef, useEffect} from 'react'
import clsx from 'clsx'
import {CircleDot} from 'lucide-react'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useToast} from '@/components/shared'
import type {Finding, TriageStatus} from '@/types'

const TRIAGE_OPTIONS: {s: TriageStatus; label: string}[] = [
  {s: 'open', label: 'Open'},
  {s: 'acknowledged', label: 'Acknowledged'},
  {s: 'in_progress', label: 'In Progress'},
  {s: 'resolved', label: 'Resolved'},
  {s: 'suppressed', label: 'Suppressed'},
]

interface Props {
  finding: Finding
}

export default function FindingTriageMenu({finding}: Props) {
  const [showTriage, setShowTriage] = useState(false)
  const triageRef = useRef<HTMLDivElement>(null)
  const toast = useToast()
  const setFindingTriage = useAnalysisStore(s => s.setFindingTriage)
  // Select only the single triage entry this card needs — selecting the whole
  // triageMap would re-render every FindingCard on any triage change (O(N)).
  const triageStatus: TriageStatus = useAnalysisStore(s => s.triageMap.get(findingKey(finding))?.status ?? 'open')

  // Close triage dropdown on click-outside so multiple cards can't have
  // dropdowns open simultaneously.
  useEffect(() => {
    if (!showTriage) return
    const handler = (e: MouseEvent) => {
      if (triageRef.current && !triageRef.current.contains(e.target as Node)) {
        setShowTriage(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [showTriage])

  const handleSetTriage = (status: TriageStatus) => {
    setShowTriage(false)
    if (status === triageStatus) return
    setFindingTriage(finding, status)
    if (status === 'suppressed') {
      toast.info('Finding suppressed', {
        action: {label: 'Undo', onClick: () => setFindingTriage(finding, 'open')},
      })
    }
  }

  return (
    <>
      {triageStatus !== 'open' && (
        <span
          className={clsx(
            'text-2xs uppercase tracking-wider px-1.5 py-0.5 rounded border',
            triageStatus === 'suppressed' && 'bg-surface-3 text-text-tertiary border-border-subtle',
            triageStatus === 'acknowledged' && 'text-blue-400 border-blue-500/30 bg-blue-500/5',
            triageStatus === 'in_progress' && 'text-amber-400 border-amber-500/30 bg-amber-500/5',
            triageStatus === 'resolved' && 'text-emerald-400 border-emerald-500/30 bg-emerald-500/5',
          )}
        >
          {triageStatus.replace('_', ' ')}
        </span>
      )}

      <div className="relative" ref={triageRef}>
        <button
          onClick={() => setShowTriage(s => !s)}
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
          title="Set triage status"
        >
          <CircleDot size={10} />
        </button>
        {showTriage && (
          <div className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-32">
            {TRIAGE_OPTIONS.map(({s, label}) => (
              <button
                key={s}
                onClick={() => handleSetTriage(s)}
                className={clsx(
                  'block w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors',
                  triageStatus === s ? 'text-brand-400 font-medium' : 'text-text-secondary',
                )}
              >
                {label}
              </button>
            ))}
          </div>
        )}
      </div>
    </>
  )
}
