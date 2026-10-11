import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useId, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Button, FormField, Input, SegmentedControl, formFieldIds, useConfirm} from '@/components/shared'
import {ApiError} from '@/api/client'
import {AzureConflictError, selectAzureScope, useAzureFoundryStore} from '@/stores/azureFoundryStore'
import type {AzureConnection, AzureFoundryAuthMode, AzureScope} from '@/types'
import AzureFormDialog from './AzureFormDialog'
import SecretField from '@/components/shared/SecretField'
import TestStatus, {IDLE_TEST, type TestState} from './TestStatus'
import {
  CONNECTION_FIELD_ORDER,
  EMPTY_CONNECTION_FORM,
  azureHost,
  connectionErrorTarget,
  connectionFormFromConnection,
  formsDiffer,
  secretCarry,
  secretField,
  toConnectionConfigInput,
  toConnectionInput,
  toConnectionUpdateInput,
  validateConnectionForm,
  withEndpoint,
  withName,
  type ConnectionFieldError,
  type ConnectionFormField,
  type ConnectionFormState,
} from './connectionForm'
import {isOwnItem} from './registryView'

export type ConnectionModalMode = 'add' | 'edit' | 'reauth'

interface AzureConnectionModalProps {
  scope: AzureScope
  mode: ConnectionModalMode
  connection: AzureConnection | null
  allowCli: boolean
  onClose: () => void
  onSaved: (connection: AzureConnection, created: boolean) => void
}

type FormAlert = {kind: 'message'; text: string} | {kind: 'conflict'} | {kind: 'forbidden'} | {kind: 'gone'}

function effective(form: ConnectionFormState): ConnectionFormState {
  return form.authMode === 'api_key' && form.projectEndpoint ? {...form, projectEndpoint: ''} : form
}

function errorMessage(err: unknown): string {
  return getErrorMessage(err, '')
}

export default function AzureConnectionModal({
  scope,
  mode,
  connection,
  allowCli,
  onClose,
  onSaved,
}: AzureConnectionModalProps) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  const createConnection = useAzureFoundryStore(s => s.createConnection)
  const updateConnection = useAzureFoundryStore(s => s.updateConnection)
  const runTest = useAzureFoundryStore(s => s.test)
  const load = useAzureFoundryStore(s => s.load)

  const [base, setBase] = useState<AzureConnection | null>(connection)
  const baseline = useMemo(() => (base ? connectionFormFromConnection(base) : EMPTY_CONNECTION_FORM), [base])
  const [form, setForm] = useState<ConnectionFormState>(baseline)
  const [touched, setTouched] = useState<ReadonlySet<ConnectionFormField>>(new Set())
  const [submitted, setSubmitted] = useState(false)
  const [serverErrors, setServerErrors] = useState<Partial<Record<ConnectionFormField, string>>>({})
  const [formAlert, setFormAlert] = useState<FormAlert | null>(null)
  const [busy, setBusy] = useState(false)
  const [test, setTest] = useState<TestState>(IDLE_TEST)

  const uid = useId()
  const ids = {
    form: `${uid}-form`,
    endpoint: `${uid}-endpoint`,
    name: `${uid}-name`,
    auth: `${uid}-auth`,
    tenantId: `${uid}-tenant`,
    clientId: `${uid}-client`,
    apiKey: `${uid}-api-key`,
    clientSecret: `${uid}-client-secret`,
    projectEndpoint: `${uid}-project`,
    testHint: `${uid}-test-hint`,
  }
  const secret = secretField(form.authMode)

  const eff = effective(form)
  const carry = secretCarry(base, eff)
  const sendsSettings =
    !base || mode === 'reauth' || toConnectionUpdateInput(eff, effective(baseline), base).config !== undefined
  const validation = (() => {
    const v = validateConnectionForm(eff, {connection: base, allowCli})
    if (sendsSettings || v.errors.secret !== 'required') return v
    const errors = {...v.errors}
    delete errors.secret
    return {...v, errors, firstInvalid: CONNECTION_FIELD_ORDER.find(f => errors[f]) ?? null}
  })()

  const fieldElement = (field: ConnectionFormField): HTMLElement | null => {
    switch (field) {
      case 'endpoint':
        return document.getElementById(ids.endpoint)
      case 'name':
        return document.getElementById(ids.name)
      case 'authMode':
        return document.getElementById(ids.auth)?.querySelector<HTMLElement>('[role="radio"][tabindex="0"]') ?? null
      case 'tenantId':
        return document.getElementById(ids.tenantId)
      case 'clientId':
        return document.getElementById(ids.clientId)
      case 'secret':
        return secret ? document.getElementById(ids[secret]) : null
      case 'projectEndpoint':
        return document.getElementById(ids.projectEndpoint)
    }
  }
  const focusField = (field: ConnectionFormField) => fieldElement(field)?.focus()

  const [initialFocusRef] = useState(() => {
    const stored = secretField(baseline.authMode)
    const target = mode !== 'reauth' || !baseline.endpoint.trim() ? ids.endpoint : stored ? ids[stored] : ids.tenantId
    return {
      get current(): HTMLElement | null {
        return document.getElementById(target)
      },
    }
  })

  const errorText = (e: ConnectionFieldError): string => {
    switch (e) {
      case 'required':
        return t('azureModels.connectionModal.errorRequired')
      case 'tooLong':
        return t('azureModels.connectionModal.errorTooLong')
      case 'cliUnavailable':
        return t('azureModels.connectionModal.errorCliUnavailable')
      default:
        return t(e)
    }
  }

  const fieldError = (field: ConnectionFormField): string | undefined => {
    if (serverErrors[field]) return serverErrors[field]
    const e = validation.errors[field]
    if (!e) return undefined
    if (field === 'authMode') return errorText(e)
    if (!submitted && (!touched.has(field) || e === 'required')) return undefined
    return errorText(e)
  }

  const touch = (field: ConnectionFormField) => setTouched(prev => (prev.has(field) ? prev : new Set(prev).add(field)))

  const change = (next: ConnectionFormState, ...fields: ConnectionFormField[]) => {
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
    const target = connectionErrorTarget(err)
    if (target === 'reindex') return 'reindex'
    const fallback = t('azureModels.connectionModal.saveFailed')
    if (target === 'conflict' || err instanceof AzureConflictError) setFormAlert({kind: 'conflict'})
    else if (target === 'forbidden') setFormAlert({kind: 'forbidden'})
    else if (err instanceof ApiError && err.code === 'REGISTRY_READ_ONLY') {
      setFormAlert({kind: 'message', text: t('azureModels.registryState.readOnly')})
    } else if (target === 'form') setFormAlert({kind: 'message', text: errorMessage(err) || fallback})
    else {
      const text =
        err instanceof ApiError && err.code === 'HOST_NOT_ALLOWED'
          ? t('providers.azureHostRejected')
          : errorMessage(err) || fallback
      const field = fieldElement(target)
      if (field) {
        setServerErrors({[target]: text})
        field.focus()
      } else {
        setFormAlert({kind: 'message', text})
      }
    }
    return 'shown'
  }

  const secretError = (): string | undefined => {
    const shown = fieldError('secret')
    return shown && !serverErrors.secret && carry === 'reenter' ? t('providers.azureSecretRequiredAfterChange') : shown
  }

  const save = async (force: boolean): Promise<void> => {
    setBusy(true)
    setFormAlert(null)
    setServerErrors({})
    try {
      let saved: AzureConnection
      if (!base) {
        saved = await createConnection(scope, toConnectionInput(eff))
      } else {
        const input = toConnectionUpdateInput(eff, effective(baseline), base)
        if (input.name === undefined && input.config === undefined) {
          setBusy(false)
          onClose()
          return
        }
        saved = await updateConnection(scope, base.id, force ? {...input, force: true} : input)
      }
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
    if (validation.firstInvalid) {
      focusField(validation.firstInvalid)
      return
    }
    void save(false)
  }

  const testDraft = async () => {
    const invalid = CONNECTION_FIELD_ORDER.find(f => f !== 'name' && validation.errors[f])
    if (invalid) {
      setSubmitted(true)
      focusField(invalid)
      return
    }
    setTest({phase: 'running'})
    try {
      const result = await runTest(scope, {
        ...(base ? {connectionId: base.id} : {}),
        config: toConnectionConfigInput(eff),
      })
      setTest(result.ok ? {phase: 'passed', latencyMs: result.latencyMs} : {phase: 'failed', error: result.error})
    } catch (err) {
      const target = connectionErrorTarget(err)
      if (target !== 'reindex' && target !== 'conflict' && target !== 'forbidden' && target !== 'form') {
        showServerError(err)
      }
      setTest({
        phase: 'failed',
        error: target === 'forbidden' ? t('azureModels.conflict.readOnly') : errorMessage(err) || undefined,
      })
    }
  }

  const reload = useCallback(async () => {
    if (!base) return
    await load(scope, {force: true})
    const fresh = selectAzureScope(scope)(useAzureFoundryStore.getState()).connections.find(
      c => c.id === base.id && isOwnItem(scope, c),
    )
    if (!fresh) {
      setFormAlert({kind: 'gone'})
      return
    }
    setBase(fresh)
    setForm(connectionFormFromConnection(fresh))
    setServerErrors({})
    setSubmitted(false)
    setTouched(new Set())
    setTest(IDLE_TEST)
    setFormAlert(null)
  }, [base, load, scope])

  const authOptions: {value: AzureFoundryAuthMode; label: string; disabled?: boolean}[] = [
    {value: 'api_key', label: t('providers.azureAuthApiKey')},
    {value: 'entra', label: t('providers.azureAuthEntra')},
    ...(allowCli || form.authMode === 'entra_cli'
      ? [{value: 'entra_cli' as const, label: t('providers.azureAuthEntraCli'), disabled: !allowCli}]
      : []),
  ]

  const endpointWarning =
    validation.endpointWarning && (submitted || touched.has('endpoint')) ? t(validation.endpointWarning) : undefined
  const endpointMoved =
    !!base &&
    base.modelCount > 0 &&
    !!azureHost(form.endpoint) &&
    azureHost(form.endpoint) !== azureHost(baseline.endpoint)
  const authError = fieldError('authMode')

  const title =
    mode === 'add'
      ? t('azureModels.connectionModal.titleAdd')
      : mode === 'reauth'
        ? t('azureModels.connectionModal.titleReauth', {name: base?.name ?? ''})
        : t('azureModels.connectionModal.titleEdit', {name: base?.name ?? ''})
  const submitLabel =
    mode === 'add'
      ? t('azureModels.connectionModal.submitAdd')
      : mode === 'reauth'
        ? t('azureModels.connectionModal.submitReauth')
        : t('azureModels.connectionModal.submitEdit')

  const alert =
    formAlert?.kind === 'conflict' ? (
      <div className="flex flex-wrap items-center gap-2">
        <span>{t('azureModels.conflict.resource')}</span>
        <Button size="sm" variant="secondary" onClick={() => void reload()}>
          {t('azureModels.conflict.reload')}
        </Button>
      </div>
    ) : formAlert?.kind === 'forbidden' ? (
      t('azureModels.conflict.readOnly')
    ) : formAlert?.kind === 'gone' ? (
      t('azureModels.connectionModal.gone')
    ) : formAlert?.kind === 'message' ? (
      formAlert.text
    ) : null

  return (
    <AzureFormDialog
      title={title}
      description={mode === 'add' ? t('azureModels.connectionModal.description') : undefined}
      size="lg"
      formId={ids.form}
      dirty={formsDiffer(form, baseline)}
      busy={busy}
      submitLabel={submitLabel}
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
            {test.phase === 'running'
              ? t('azureModels.connectionModal.testing')
              : t('azureModels.connectionModal.test')}
          </Button>
          <p id={ids.testHint} className="text-2xs text-text-tertiary">
            {t('azureModels.connectionModal.testHint')}
          </p>
          <TestStatus state={test} />
        </div>
      }
    >
      <FormField id={ids.endpoint} label={t('azureModels.connectionModal.endpoint')}>
        <Input
          id={ids.endpoint}
          value={form.endpoint}
          onChange={e => change(withEndpoint(form, e.target.value), 'endpoint', 'name')}
          onBlur={() => touch('endpoint')}
          placeholder={t('providers.azureEndpointPlaceholder')}
          error={fieldError('endpoint')}
          hint={endpointWarning ?? t('providers.azureEndpointHint')}
          autoComplete="off"
          spellCheck={false}
          inputMode="url"
        />
      </FormField>
      {endpointMoved && (
        <p className="text-xs text-text-secondary">
          {t('azureModels.connectionModal.endpointChange', {count: base?.modelCount ?? 0})}
        </p>
      )}

      <FormField id={ids.name} label={t('azureModels.connectionModal.name')}>
        <Input
          id={ids.name}
          value={form.name}
          onChange={e => change(withName(form, e.target.value), 'name')}
          onBlur={() => touch('name')}
          error={fieldError('name')}
          hint={t('azureModels.connectionModal.nameHint')}
          autoComplete="off"
          spellCheck={false}
        />
      </FormField>

      <FormField id={ids.auth} label={t('providers.azureAuthMode')} error={authError}>
        <div id={ids.auth}>
          <SegmentedControl
            size="sm"
            value={form.authMode}
            onChange={authMode => change({...form, authMode}, 'authMode', 'secret', 'tenantId', 'clientId')}
            options={authOptions}
            labelledBy={formFieldIds(ids.auth).labelId}
            describedBy={authError ? formFieldIds(ids.auth).errorId : undefined}
            activation="manual"
            className="flex-wrap"
          />
        </div>
      </FormField>

      {secret === 'apiKey' ? (
        <SecretField
          id={ids.apiKey}
          label={t('providers.azureApiKey')}
          value={form.apiKey}
          onChange={apiKey => change({...form, apiKey}, 'secret')}
          placeholder={carry === 'kept' ? t('providers.azureSecretUnchanged') : t('apiKey.placeholder')}
          hint={carry === 'reenter' ? t('providers.azureSecretRequiredAfterChange') : undefined}
          error={secretError()}
        />
      ) : secret === null ? (
        <FormField id={ids.tenantId} label={t('providers.azureTenantId')}>
          <Input
            id={ids.tenantId}
            value={form.tenantId}
            onChange={e => change({...form, tenantId: e.target.value}, 'tenantId')}
            onBlur={() => touch('tenantId')}
            error={fieldError('tenantId')}
            hint={t('providers.azureEntraCliTenantHint')}
            autoComplete="off"
            spellCheck={false}
          />
        </FormField>
      ) : (
        <>
          <FormField id={ids.tenantId} label={t('providers.azureTenantId')}>
            <Input
              id={ids.tenantId}
              value={form.tenantId}
              onChange={e => change({...form, tenantId: e.target.value}, 'tenantId', 'secret')}
              onBlur={() => touch('tenantId')}
              error={fieldError('tenantId')}
              autoComplete="off"
              spellCheck={false}
            />
          </FormField>
          <FormField id={ids.clientId} label={t('providers.azureClientId')}>
            <Input
              id={ids.clientId}
              value={form.clientId}
              onChange={e => change({...form, clientId: e.target.value}, 'clientId', 'secret')}
              onBlur={() => touch('clientId')}
              error={fieldError('clientId')}
              autoComplete="off"
              spellCheck={false}
            />
          </FormField>
          <SecretField
            id={ids.clientSecret}
            label={t('providers.azureClientSecret')}
            value={form.clientSecret}
            onChange={clientSecret => change({...form, clientSecret}, 'secret')}
            placeholder={carry === 'kept' ? t('providers.azureSecretUnchanged') : undefined}
            hint={carry === 'reenter' ? t('providers.azureSecretRequiredAfterChange') : undefined}
            error={secretError()}
          />
        </>
      )}

      {secret === 'apiKey' ? (
        <p className="text-xs text-text-tertiary">{t('azureModels.connectionModal.apiKeyInfo')}</p>
      ) : (
        <FormField id={ids.projectEndpoint} label={t('providers.azureProjectEndpoint')}>
          <Input
            id={ids.projectEndpoint}
            value={form.projectEndpoint}
            onChange={e => change({...form, projectEndpoint: e.target.value}, 'projectEndpoint', 'secret')}
            onBlur={() => touch('projectEndpoint')}
            placeholder={t('providers.azureProjectEndpointPlaceholder')}
            error={fieldError('projectEndpoint')}
            hint={t('providers.azureProjectEndpointHint')}
            autoComplete="off"
            spellCheck={false}
            inputMode="url"
          />
        </FormField>
      )}
    </AzureFormDialog>
  )
}
