import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useId, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {
  Button,
  FormField,
  Input,
  SegmentedControl,
  Select,
  Textarea,
  formFieldIds,
  useConfirm,
} from '@/components/shared'
import {ApiError} from '@/api/client'
import {AzureConflictError, selectAzureScope, useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {PURPOSE_CUSTOM_MAX_LENGTH, PURPOSE_PRESETS, modelKindLabelKey, purposeLabelKey} from '@/lib/modelPurpose'
import {formatCount} from '@/lib/format'
import type {
  AzureConnection,
  AzureModelEntry,
  AzureModelKind,
  AzureScope,
  ModelPurpose,
  ModelPurposePreset,
} from '@/types'
import AzureFormDialog from './AzureFormDialog'
import TestStatus, {IDLE_TEST, type TestState} from './TestStatus'
import {
  CONTEXT_WINDOW_MAX,
  CONTEXT_WINDOW_MIN,
  DESCRIPTION_MAX_LENGTH,
  DISPLAY_NAME_MAX_LENGTH,
  MAX_OUTPUT_MAX,
  MAX_OUTPUT_MIN,
  MODEL_FIELD_ORDER,
  cleanLength,
  inferKind,
  modelErrorTarget,
  modelFormFromEntry,
  modelFormsDiffer,
  toModelInput,
  toModelUpdateInput,
  validateModelForm,
  withDeployment,
  withDisplayName,
  type ModelFieldError,
  type ModelFormField,
  type ModelFormState,
} from './modelForm'
import {findDisplayNameTwin, findDuplicateModel, isOwnItem, unaddedItems} from './registryView'

interface AzureModelModalProps {
  scope: AzureScope
  entry: AzureModelEntry | null
  initial: ModelFormState
  reportedWindow?: number
  connections: AzureConnection[]
  models: AzureModelEntry[]
  onClose: () => void
  onSaved: (entry: AzureModelEntry, created: boolean) => void
}

type FormAlert = {kind: 'message'; text: string} | {kind: 'conflict'} | {kind: 'forbidden'} | {kind: 'gone'}

type FieldProblem = ModelFieldError | 'duplicate'

const KINDS: readonly AzureModelKind[] = ['chat', 'claude', 'agent', 'embedding']
const ADVANCED_FIELDS: readonly ModelFormField[] = ['contextWindow', 'maxOutputTokens']
const MAX_CHIPS = 8

function errorMessage(err: unknown): string {
  return getErrorMessage(err, '')
}

export default function AzureModelModal({
  scope,
  entry,
  initial,
  reportedWindow,
  connections,
  models,
  onClose,
  onSaved,
}: AzureModelModalProps) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  const createModel = useAzureFoundryStore(s => s.createModel)
  const updateModel = useAzureFoundryStore(s => s.updateModel)
  const runTest = useAzureFoundryStore(s => s.test)
  const load = useAzureFoundryStore(s => s.load)

  const [base, setBase] = useState<AzureModelEntry | null>(entry)
  const [baseline, setBaseline] = useState<ModelFormState>(initial)
  const [form, setForm] = useState<ModelFormState>(initial)
  const [touched, setTouched] = useState<ReadonlySet<ModelFormField>>(new Set())
  const [submitted, setSubmitted] = useState(false)
  const [serverErrors, setServerErrors] = useState<Partial<Record<ModelFormField, string>>>({})
  const [formAlert, setFormAlert] = useState<FormAlert | null>(null)
  const [busy, setBusy] = useState(false)
  const [test, setTest] = useState<TestState>(IDLE_TEST)
  const [advancedOpen, setAdvancedOpen] = useState(!!initial.contextWindow || !!initial.maxOutputTokens)
  const advancedRef = useRef<HTMLDetailsElement>(null)

  const discovery = useAzureFoundryStore(s => selectAzureScope(scope)(s).discovery[form.connectionId]?.data ?? null)

  const labelOf = useCallback((p: ModelPurposePreset) => t(purposeLabelKey(p), {ns: 'common'}), [t])
  const kindLabel = (k: AzureModelKind) => t(modelKindLabelKey(k), {ns: 'common'})

  const uid = useId()
  const ids = {
    form: `${uid}-form`,
    connectionId: `${uid}-resource`,
    kind: `${uid}-kind`,
    deployment: `${uid}-deployment`,
    chips: `${uid}-chips`,
    displayName: `${uid}-display-name`,
    purpose: `${uid}-purpose`,
    purposeCustom: `${uid}-purpose-custom`,
    description: `${uid}-description`,
    contextWindow: `${uid}-context-window`,
    maxOutputTokens: `${uid}-max-output`,
    testHint: `${uid}-test-hint`,
  }

  const connection = connections.find(c => c.id === form.connectionId)
  const validation = validateModelForm(form, {supportsAgents: connection?.supportsAgents, existing: base, labelOf})
  const duplicate = findDuplicateModel(models, form, base?.id)
  const problems: Partial<Record<ModelFormField, FieldProblem>> = {...validation.errors}
  if (!problems.deployment && duplicate) problems.deployment = 'duplicate'
  const firstInvalid = MODEL_FIELD_ORDER.find(f => !!problems[f])

  const fieldElement = (field: ModelFormField): HTMLElement | null => {
    if (field === 'kind') {
      return document.getElementById(ids.kind)?.querySelector<HTMLElement>('[role="radio"][tabindex="0"]') ?? null
    }
    if (field === 'purpose' && form.purpose === 'custom') return document.getElementById(ids.purposeCustom)
    return document.getElementById(ids[field])
  }
  const focusField = (field: ModelFormField) => {
    if (ADVANCED_FIELDS.includes(field) && advancedRef.current && !advancedRef.current.open) {
      advancedRef.current.open = true
      setAdvancedOpen(true)
    }
    fieldElement(field)?.focus()
  }

  const [initialFocusRef] = useState(() => {
    const target = connections.length > 1 ? ids.connectionId : ids.deployment
    return {
      get current(): HTMLElement | null {
        return document.getElementById(target)
      },
    }
  })

  const errorText = (field: ModelFormField, e: FieldProblem): string => {
    switch (e) {
      case 'required':
        return t('azureModels.modelModal.errorRequired')
      case 'tooLong':
        return t('azureModels.modelModal.errorTooLong', {
          max:
            field === 'displayName'
              ? DISPLAY_NAME_MAX_LENGTH
              : field === 'purpose'
                ? PURPOSE_CUSTOM_MAX_LENGTH
                : DESCRIPTION_MAX_LENGTH,
        })
      case 'invalidName':
        return form.kind === 'agent'
          ? t('azureModels.modelModal.errorInvalidAgentName')
          : t('azureModels.modelModal.errorInvalidName')
      case 'kindNeedsProject':
        return t('azureModels.modelModal.errorKindNeedsProject')
      case 'notInteger':
        return t('azureModels.modelModal.errorNotInteger')
      case 'outOfRange':
        return field === 'contextWindow'
          ? t('azureModels.modelModal.errorOutOfRange', {
              min: formatCount(CONTEXT_WINDOW_MIN),
              max: formatCount(CONTEXT_WINDOW_MAX),
            })
          : t('azureModels.modelModal.errorOutOfRange', {
              min: formatCount(MAX_OUTPUT_MIN),
              max: formatCount(MAX_OUTPUT_MAX),
            })
      case 'duplicate':
        return t('azureModels.modelModal.errorDuplicate')
    }
  }

  const fieldError = (field: ModelFormField): string | undefined => {
    if (serverErrors[field]) return serverErrors[field]
    const e = problems[field]
    if (!e) return undefined
    if (field !== 'kind' && !submitted && (!touched.has(field) || e === 'required')) return undefined
    return errorText(field, e)
  }

  const touch = (field: ModelFormField) => setTouched(prev => (prev.has(field) ? prev : new Set(prev).add(field)))

  const change = (next: ModelFormState, ...fields: ModelFormField[]) => {
    setForm(next)
    if (fields.some(f => serverErrors[f])) {
      setServerErrors(prev => {
        const out = {...prev}
        for (const f of fields) delete out[f]
        return out
      })
    }
    if (test.phase !== 'idle' && test.phase !== 'running') setTest(IDLE_TEST)
  }

  const showServerError = (err: unknown): 'reindex' | 'shown' => {
    const target = modelErrorTarget(err)
    if (target === 'reindex') return 'reindex'
    if (err instanceof AzureConflictError) setFormAlert({kind: 'conflict'})
    else if (target === 'forbidden') setFormAlert({kind: 'forbidden'})
    else if (err instanceof ApiError && err.code === 'REGISTRY_READ_ONLY') {
      setFormAlert({kind: 'message', text: t('azureModels.registryState.readOnly')})
    } else {
      const text = errorMessage(err) || t('azureModels.modelModal.saveFailed')
      if (target !== 'form' && fieldElement(target)) {
        setServerErrors({[target]: text})
        focusField(target)
      } else {
        setFormAlert({kind: 'message', text})
      }
    }
    return 'shown'
  }

  const save = async (force: boolean): Promise<void> => {
    setBusy(true)
    setFormAlert(null)
    setServerErrors({})
    try {
      const saved = base
        ? await updateModel(scope, base.id, {
            ...toModelUpdateInput(form, labelOf, base.updatedAt),
            ...(force ? {force: true} : {}),
          })
        : await createModel(scope, {...toModelInput(form, labelOf), ...(force ? {force: true} : {})})
      setBusy(false)
      onSaved(saved, !base)
    } catch (err) {
      setBusy(false)
      if (showServerError(err) !== 'reindex') return
      const ok = await confirm({
        title: t('azureModels.knowledge.reindexTitle'),
        message: t('azureModels.knowledge.reindexMessage'),
        confirmLabel: t('azureModels.knowledge.reindexConfirm'),
      })
      if (ok) await save(true)
    }
  }

  const submit = () => {
    setSubmitted(true)
    if (firstInvalid) {
      focusField(firstInvalid)
      return
    }
    if (base && !modelFormsDiffer(form, baseline)) {
      onClose()
      return
    }
    void save(false)
  }

  const testDraft = async () => {
    const invalid = (['connectionId', 'kind', 'deployment'] as const).find(f => validation.errors[f])
    if (invalid) {
      setSubmitted(true)
      focusField(invalid)
      return
    }
    setTest({phase: 'running'})
    try {
      const result = await runTest(scope, {
        connectionId: form.connectionId,
        model: {kind: form.kind, deployment: form.deployment.trim()},
      })
      setTest(result.ok ? {phase: 'passed', latencyMs: result.latencyMs} : {phase: 'failed', error: result.error})
    } catch (err) {
      const target = modelErrorTarget(err)
      if (target !== 'reindex' && target !== 'forbidden' && target !== 'form' && fieldElement(target)) {
        setServerErrors({[target]: errorMessage(err) || t('azureModels.test.failedGeneric')})
      }
      setTest({
        phase: 'failed',
        error: target === 'forbidden' ? t('azureModels.conflict.readOnly') : errorMessage(err) || undefined,
      })
    }
  }

  const reload = async () => {
    if (!base) return
    await load(scope, {force: true})
    const fresh = selectAzureScope(scope)(useAzureFoundryStore.getState()).models.find(
      m => m.id === base.id && isOwnItem(scope, m),
    )
    if (!fresh) {
      setFormAlert({kind: 'gone'})
      return
    }
    const next = modelFormFromEntry(fresh)
    if (!next) {
      setFormAlert({kind: 'message', text: t('azureModels.model.editUnknownKind')})
      return
    }
    setBase(fresh)
    setBaseline(next)
    setForm(next)
    setServerErrors({})
    setSubmitted(false)
    setTouched(new Set())
    setTest(IDLE_TEST)
    setFormAlert(null)
  }

  const chips = useMemo(
    () =>
      base
        ? []
        : unaddedItems(discovery)
            .filter(item => (item.kind === 'agent') === (form.kind === 'agent'))
            .slice(0, MAX_CHIPS),
    [base, discovery, form.kind],
  )

  const suggestedKind = inferKind(form.deployment)
  const showKindSuggestion = !!suggestedKind && suggestedKind !== form.kind && form.kind !== 'agent'
  const agentUnavailable = connection ? !connection.supportsAgents : false
  const kindError = fieldError('kind')
  const nameLabel =
    form.kind === 'agent' ? t('azureModels.modelModal.agentName') : t('azureModels.modelModal.deployment')
  const displayTwin = findDisplayNameTwin(models, form.displayName || form.deployment, base?.id)
  const embedding = form.kind === 'embedding'
  const reported = reportedWindow && reportedWindow > 0 ? reportedWindow : undefined

  const alert =
    formAlert?.kind === 'conflict' ? (
      <div className="flex flex-wrap items-center gap-2">
        <span>{t('azureModels.conflict.model')}</span>
        <Button size="sm" variant="secondary" onClick={() => void reload()}>
          {t('azureModels.conflict.reload')}
        </Button>
      </div>
    ) : formAlert?.kind === 'forbidden' ? (
      t('azureModels.conflict.readOnly')
    ) : formAlert?.kind === 'gone' ? (
      t('azureModels.modelModal.gone')
    ) : formAlert?.kind === 'message' ? (
      formAlert.text
    ) : null

  const purposeOptions = [...PURPOSE_PRESETS, 'custom' as const].map(p => ({
    value: p,
    label: t(purposeLabelKey(p), {ns: 'common'}),
  }))

  return (
    <AzureFormDialog
      title={
        base ? t('azureModels.modelModal.titleEdit', {name: base.displayName}) : t('azureModels.modelModal.titleAdd')
      }
      description={base ? undefined : t('azureModels.modelModal.description')}
      size="md"
      formId={ids.form}
      dirty={modelFormsDiffer(form, baseline)}
      busy={busy}
      submitLabel={base ? t('azureModels.modelModal.submitEdit') : t('azureModels.modelModal.submitAdd')}
      onSubmit={submit}
      onClose={onClose}
      formAlert={alert}
      initialFocusRef={initialFocusRef}
      footerStart={
        <div className="space-y-1">
          <Button
            size="sm"
            variant="secondary"
            onClick={() => void testDraft()}
            disabled={test.phase === 'running' || busy}
            aria-describedby={ids.testHint}
          >
            {test.phase === 'running' ? t('azureModels.modelModal.testing') : t('azureModels.modelModal.test')}
          </Button>
          <p id={ids.testHint} className="text-2xs text-text-tertiary">
            {form.kind === 'agent'
              ? t('azureModels.modelModal.testHintAgent')
              : t('azureModels.modelModal.testHintBilled')}
          </p>
          <TestStatus state={test} />
        </div>
      }
    >
      {connections.length > 1 ? (
        <FormField id={ids.connectionId} label={t('azureModels.modelModal.resource')}>
          <Select
            id={ids.connectionId}
            value={form.connectionId}
            onChange={e => change({...form, connectionId: e.target.value}, 'connectionId', 'kind', 'deployment')}
            options={connections.map(c => ({value: c.id, label: c.name}))}
            error={fieldError('connectionId')}
          />
        </FormField>
      ) : (
        <div className="space-y-1">
          <p className="text-xs font-medium text-text-secondary">{t('azureModels.modelModal.resource')}</p>
          <p className="text-sm text-text-primary">{connection?.name ?? form.connectionId}</p>
        </div>
      )}

      <FormField
        id={ids.kind}
        label={t('azureModels.modelModal.kind')}
        hint={agentUnavailable ? t('azureModels.modelModal.kindAgentUnavailable') : undefined}
        error={kindError}
      >
        <div id={ids.kind}>
          <SegmentedControl
            size="sm"
            value={form.kind}
            onChange={kind => change({...form, kind}, 'kind', 'deployment')}
            options={KINDS.map(k => ({value: k, label: kindLabel(k), disabled: k === 'agent' && agentUnavailable}))}
            labelledBy={formFieldIds(ids.kind).labelId}
            describedBy={
              [
                agentUnavailable ? formFieldIds(ids.kind).hintId : undefined,
                kindError ? formFieldIds(ids.kind).errorId : undefined,
              ]
                .filter(Boolean)
                .join(' ') || undefined
            }
            activation="manual"
            className="flex-wrap"
          />
        </div>
      </FormField>

      <FormField id={ids.deployment} label={nameLabel}>
        <Input
          id={ids.deployment}
          value={form.deployment}
          onChange={e => change(withDeployment(form, e.target.value), 'deployment', 'displayName')}
          onBlur={() => touch('deployment')}
          error={fieldError('deployment')}
          hint={
            form.kind === 'agent' ? t('azureModels.modelModal.agentHint') : t('azureModels.modelModal.deploymentHint')
          }
          autoComplete="off"
          spellCheck={false}
        />
      </FormField>
      {showKindSuggestion && suggestedKind && (
        <div className="flex flex-wrap items-center gap-2 text-xs text-text-secondary">
          <span>{t('azureModels.modelModal.kindSuggest', {kind: kindLabel(suggestedKind)})}</span>
          <Button size="sm" variant="ghost" onClick={() => change({...form, kind: suggestedKind}, 'kind')}>
            {t('azureModels.modelModal.kindSwitch', {kind: kindLabel(suggestedKind)})}
          </Button>
        </div>
      )}
      {chips.length > 0 && (
        <div role="group" aria-labelledby={ids.chips} className="space-y-1">
          <p id={ids.chips} className="text-xs font-medium text-text-secondary">
            {t('azureModels.modelModal.found')}
          </p>
          <div className="flex flex-wrap gap-1.5">
            {chips.map(item => (
              <Button
                key={`${item.kind}:${item.name}`}
                size="sm"
                variant="secondary"
                aria-label={t('azureModels.modelModal.foundUse', {name: item.name})}
                onClick={() => change({...withDeployment(form, item.name), kind: item.kind}, 'deployment', 'kind')}
              >
                {item.name}
              </Button>
            ))}
          </div>
        </div>
      )}

      <FormField id={ids.displayName} label={t('azureModels.modelModal.displayName')}>
        <Input
          id={ids.displayName}
          value={form.displayName}
          onChange={e => change(withDisplayName(form, e.target.value), 'displayName')}
          onBlur={() => touch('displayName')}
          error={fieldError('displayName')}
          hint={displayTwin ? t('azureModels.modelModal.displayNameTwin') : t('azureModels.modelModal.displayNameHint')}
          autoComplete="off"
        />
      </FormField>

      <FormField id={ids.purpose} label={t('azureModels.modelModal.purpose')}>
        <Select
          id={ids.purpose}
          value={form.purpose}
          onChange={e => change({...form, purpose: e.target.value as ModelPurpose}, 'purpose')}
          options={purposeOptions}
          hint={form.purpose === 'custom' ? undefined : t('azureModels.modelModal.purposeHint')}
        />
      </FormField>
      {form.purpose === 'custom' && (
        <FormField
          id={ids.purposeCustom}
          label={t('azureModels.modelModal.purposeCustom')}
          aside={t('azureModels.modelModal.counter', {
            count: cleanLength(form.purposeCustom),
            max: PURPOSE_CUSTOM_MAX_LENGTH,
          })}
        >
          <Input
            id={ids.purposeCustom}
            value={form.purposeCustom}
            onChange={e => change({...form, purposeCustom: e.target.value}, 'purpose')}
            onBlur={() => touch('purpose')}
            error={fieldError('purpose')}
            autoComplete="off"
          />
        </FormField>
      )}

      <FormField
        id={ids.description}
        label={t('azureModels.modelModal.descriptionLabel')}
        aside={t('azureModels.modelModal.counter', {
          count: cleanLength(form.description),
          max: DESCRIPTION_MAX_LENGTH,
        })}
      >
        <Textarea
          id={ids.description}
          value={form.description}
          onChange={e => change({...form, description: e.target.value}, 'description')}
          onBlur={() => touch('description')}
          error={fieldError('description')}
          hint={t('azureModels.modelModal.descriptionHint')}
        />
      </FormField>

      {!embedding && (
        <details
          ref={advancedRef}
          open={advancedOpen}
          onToggle={e => setAdvancedOpen(e.currentTarget.open)}
          className="rounded-md border border-border-subtle px-3 py-2"
        >
          <summary className="cursor-pointer text-sm font-medium text-text-secondary">
            {t('azureModels.modelModal.advanced')}
          </summary>
          <div className="mt-3 space-y-4">
            <FormField id={ids.contextWindow} label={t('azureModels.modelModal.contextWindow')}>
              <Input
                id={ids.contextWindow}
                value={form.contextWindow}
                onChange={e => change({...form, contextWindow: e.target.value}, 'contextWindow')}
                onBlur={() => touch('contextWindow')}
                placeholder={reported ? String(reported) : undefined}
                error={fieldError('contextWindow')}
                hint={
                  reported
                    ? t('azureModels.modelModal.contextWindowReported', {window: formatCount(reported)})
                    : t('azureModels.modelModal.contextWindowUnknown')
                }
                inputMode="numeric"
                autoComplete="off"
              />
            </FormField>
            <FormField id={ids.maxOutputTokens} label={t('azureModels.modelModal.maxOutput')}>
              <Input
                id={ids.maxOutputTokens}
                value={form.maxOutputTokens}
                onChange={e => change({...form, maxOutputTokens: e.target.value}, 'maxOutputTokens')}
                onBlur={() => touch('maxOutputTokens')}
                error={fieldError('maxOutputTokens')}
                hint={t('azureModels.modelModal.maxOutputHint')}
                inputMode="numeric"
                autoComplete="off"
              />
            </FormField>
          </div>
        </details>
      )}
    </AzureFormDialog>
  )
}
