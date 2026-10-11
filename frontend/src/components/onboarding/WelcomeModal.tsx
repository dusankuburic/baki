import {useState, useCallback, useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import {FlaskConical, Search, Wrench, Download, Sparkles, ChevronRight, X} from 'lucide-react'
import Modal from '@/components/shared/Modal'
import {Spinner, useToast} from '@/components/shared'
import {useSettingsStore} from '@/stores/settingsStore'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi, analysisApi, providersApi} from '@/api'
import {SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES} from '@/data/sampleFlow'
import {useUIStore} from '@/stores/uiStore'
import {getErrorMessage} from '@/lib/getErrorMessage'

interface WelcomeModalProps {
  isOpen: boolean
  onClose: () => void
}

interface StepDef {
  icon: typeof FlaskConical
  title: string
  body: (ruleCount: number | null) => string
}

const STEPS: StepDef[] = [
  {
    icon: FlaskConical,
    title: 'Open a flow',
    body: () =>
      'Drag a PAD text export onto the window, pick one from the sidebar, or start with the bundled sample flow.',
  },
  {
    icon: Search,
    title: 'Run analysis',
    body: count => {
      const n = count != null ? `${count}` : 'dozens of'
      return `One click runs ${n} static-analysis rules covering security, reliability, and style. Findings appear grouped by rule.`
    },
  },
  {
    icon: Wrench,
    title: 'Apply fixes',
    body: () =>
      'Many rule findings have a deterministic one-click auto-fix (preview the diff first). Select multiple to bulk-fix.',
  },
  {
    icon: Download,
    title: 'Export',
    body: () => 'Export findings as SARIF for CI, or a PDF/Markdown report. The CLI (bakicli) gates CI on severity.',
  },
  {
    icon: Sparkles,
    title: 'Explain & fix with AI',
    body: () =>
      'Connect an AI provider key (Settings → AI Accounts) to explain findings, fix with AI, and chat about your flows. Analysis and auto-fix work without one.',
  },
]

export default function WelcomeModal({isOpen, onClose}: WelcomeModalProps) {
  const {t} = useTranslation('shell')
  const [step, setStep] = useState(0)
  const [loadingSample, setLoadingSample] = useState(false)
  const [ruleCount, setRuleCount] = useState<number | null>(null)
  const setDocument = useFlowStore(s => s.setDocument)
  const updateGeneral = useSettingsStore(s => s.updateGeneral)
  const {error: toastError} = useToast()
  const isLast = step === STEPS.length - 1

  const [aiConfigured, setAiConfigured] = useState<boolean | null>(null)
  useEffect(() => {
    if (!isOpen) return
    let cancelled = false
    providersApi
      .listProviders()
      .then((ps: {configured?: boolean}[]) => {
        if (!cancelled) setAiConfigured((ps || []).some(p => p.configured))
      })
      .catch(() => {
        if (!cancelled) setAiConfigured(null)
      })
    return () => {
      cancelled = true
    }
  }, [isOpen])

  useEffect(() => {
    if (!isOpen) return
    let cancelled = false
    analysisApi
      .getRules()
      .then(rules => {
        if (!cancelled && Array.isArray(rules)) setRuleCount(rules.length)
      })
      .catch(() => {
      })
    return () => {
      cancelled = true
    }
  }, [isOpen])

  const complete = useCallback(() => {
    void updateGeneral({firstRunCompleted: true})
    onClose()
  }, [updateGeneral, onClose])

  const handleStart = useCallback(async () => {
    setLoadingSample(true)
    try {
      const doc = await flowApi.uploadFlow(SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES)
      if (doc) setDocument(doc)
    } catch (e) {
      toastError(t('toasts.loadSampleFailed'), {description: getErrorMessage(e)})
    } finally {
      setLoadingSample(false)
    }
    complete()
  }, [setDocument, complete, t, toastError])

  const handleSkip = useCallback(() => {
    complete()
  }, [complete])

  const current = STEPS[step]
  if (!current) return null
  const Icon = current.icon

  return (
    <Modal
      isOpen={isOpen}
      onClose={handleSkip}
      ariaLabel="Welcome to PAD Analyzer"
      size="md"
      closeOnEsc
      closeOnBackdrop={false}
    >
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
        <button
          onClick={handleSkip}
          className="text-text-tertiary hover:text-text-secondary p-1"
          aria-label="Skip tour"
        >
          <X size={16} />
        </button>
      </div>

      <p className="text-sm text-text-secondary leading-relaxed mb-6 min-h-[3rem]">{current.body(ruleCount)}</p>

      {current.title.startsWith('Explain') && aiConfigured === false && (
        <div className="flex items-center justify-between gap-3 mb-6 px-3 py-2 rounded-lg border border-brand-500/30 bg-brand-500/5">
          <span className="text-xs text-text-secondary">No AI provider connected yet.</span>
          <button
            onClick={() => {
              useUIStore.getState().openSettings('accounts')
              complete()
            }}
            className="flex items-center gap-1 text-xs font-medium text-brand-400 hover:text-brand-300"
          >
            Set one up
            <ChevronRight size={12} />
          </button>
        </div>
      )}

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
