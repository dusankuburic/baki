import {useState, useCallback} from 'react'
import {FlaskConical, Search, Wrench, Download, ChevronRight, X} from 'lucide-react'
import Modal from '@/components/shared/Modal'
import {Spinner} from '@/components/shared'
import {useSettingsStore} from '@/stores/settingsStore'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi} from '@/api'
import {SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES} from '@/data/sampleFlow'

interface WelcomeModalProps {
  isOpen: boolean
  onClose: () => void
}

const STEPS = [
  {
    icon: FlaskConical,
    title: 'Open a flow',
    body: 'Drag a PAD text export onto the window, pick one from the sidebar, or start with the bundled sample flow.',
  },
  {
    icon: Search,
    title: 'Run analysis',
    body: 'One click runs 29 static-analysis rules covering security, reliability, and style. Findings appear grouped by rule.',
  },
  {
    icon: Wrench,
    title: 'Apply fixes',
    body: '17 rule findings have a deterministic one-click auto-fix (preview the diff first). Select multiple to bulk-fix.',
  },
  {
    icon: Download,
    title: 'Export',
    body: 'Export findings as SARIF for CI, or a PDF/Markdown report. The CLI (bakicli) gates CI on severity.',
  },
]

export default function WelcomeModal({isOpen, onClose}: WelcomeModalProps) {
  const [step, setStep] = useState(0)
  const [loadingSample, setLoadingSample] = useState(false)
  const setDocument = useFlowStore(s => s.setDocument)
  const updateGeneral = useSettingsStore(s => s.updateGeneral)
  const current = STEPS[step]
  const isLast = step === STEPS.length - 1

  const complete = useCallback(() => {
    updateGeneral({firstRunCompleted: true})
    onClose()
  }, [updateGeneral, onClose])

  const handleStart = useCallback(async () => {
    // Load the sample flow so the user lands on something concrete.
    setLoadingSample(true)
    try {
      const doc = await flowApi.uploadFlow(SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES)
      if (doc) setDocument(doc)
    } catch {
      // Sample failed — still complete onboarding so the user isn't stuck.
    } finally {
      setLoadingSample(false)
    }
    complete()
  }, [setDocument, complete])

  const handleSkip = useCallback(() => {
    setStep(STEPS.length - 1)
  }, [])

  const Icon = current.icon

  return (
    <Modal isOpen={isOpen} onClose={handleSkip} ariaLabel="Welcome to PAD Analyzer" size="md" closeOnEsc closeOnBackdrop={false}>
      <div className="flex items-start justify-between mb-4">
        <div className="flex items-center gap-3">
          <div className="w-10 h-10 rounded-lg bg-brand-500/10 flex items-center justify-center text-brand-400">
            <Icon size={20} />
          </div>
          <div>
            <h2 className="text-base font-semibold text-text-primary">{current.title}</h2>
            <p className="text-2xs text-text-tertiary">
              Step {step + 1} of {STEPS.length}
            </p>
          </div>
        </div>
        <button onClick={handleSkip} className="text-text-tertiary hover:text-text-secondary p-1" aria-label="Skip tour">
          <X size={16} />
        </button>
      </div>

      <p className="text-sm text-text-secondary leading-relaxed mb-6 min-h-[3rem]">{current.body}</p>

      {/* Progress dots */}
      <div className="flex gap-1.5 mb-6">
        {STEPS.map((_, i) => (
          <button
            key={i}
            onClick={() => setStep(i)}
            className={`h-1.5 rounded-full transition-all ${i === step ? 'w-6 bg-brand-500' : 'w-1.5 bg-border-default hover:bg-border-strong'}`}
            aria-label={`Go to step ${i + 1}`}
          />
        ))}
      </div>

      <div className="flex items-center justify-between">
        <button onClick={handleSkip} className="text-xs text-text-tertiary hover:text-text-secondary">
          Skip
        </button>
        {isLast ? (
          <button
            onClick={handleStart}
            disabled={loadingSample}
            className="flex items-center gap-2 px-4 py-1.5 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors disabled:opacity-50"
          >
            {loadingSample ? <Spinner size={14} /> : <FlaskConical size={14} />}
            {loadingSample ? 'Loading…' : 'Load sample & start'}
          </button>
        ) : (
          <button
            onClick={() => setStep(s => s + 1)}
            className="flex items-center gap-1.5 px-4 py-1.5 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors"
          >
            Next
            <ChevronRight size={14} />
          </button>
        )}
      </div>
    </Modal>
  )
}
