import {useCallback, useId, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Plus, Trash2, Pencil, ShieldCheck, ShieldOff, AlertTriangle} from 'lucide-react'
import {orgRulesApi, isNewerRule, type OrgCustomRule, type OrgCustomRuleConfig} from '@/api/governance'
import {analysisApi, libraryApi} from '@/api'
import {ApiError} from '@/api/client'
import type {CustomRuleValidationEntry} from '@/api/analysis'
import {Button, useToast, useConfirm} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {logger} from '@/lib/logger'
import {severityTone} from '@/lib/severityTone'
import clsx from 'clsx'
import {getErrorMessage} from '@/lib/getErrorMessage'

const SEVERITIES: OrgCustomRuleConfig['severity'][] = ['error', 'warning', 'info']

const emptyRule = (): OrgCustomRuleConfig => ({
  kind: 'block',
  schemaVersion: 1,
  id: '',
  name: '',
  description: '',
  severity: 'warning',
  category: 'Style',
  rawTypeMatch: '',
})

const FORM_FIELDS = ['id', 'name', 'description', 'rawTypeMatch', 'nameMatch', 'suggestion', 'severity'] as const
type FormField = (typeof FORM_FIELDS)[number]
const isFormField = (field?: string): field is FormField =>
  !!field && (FORM_FIELDS as readonly string[]).includes(field)

interface Verdict {
  valid: boolean
  fieldErrors: Partial<Record<FormField, string>>
  errors: string[]
  warnings: string[]
}

const invalidVerdict = (message: string, field?: string): Verdict =>
  isFormField(field)
    ? {valid: false, fieldErrors: {[field]: message}, errors: [], warnings: []}
    : {valid: false, fieldErrors: {}, errors: [message], warnings: []}

function verdictOf(entry: CustomRuleValidationEntry | undefined, editing: boolean): Verdict {
  const v: Verdict = {valid: !!entry, fieldErrors: {}, errors: [], warnings: []}
  if (!entry) return v
  const problems =
    entry.problems ?? (entry.error ? [{field: entry.field, reason: entry.reason ?? '', message: entry.error}] : [])
  for (const p of problems) {
    if (p.warning || (editing && p.reason === 'RESERVED_ID')) {
      v.warnings.push(p.message)
      continue
    }
    v.valid = false
    if (isFormField(p.field) && !v.fieldErrors[p.field]) v.fieldErrors[p.field] = p.message
    else v.errors.push(p.message)
  }
  return v
}

function fieldErrorOf(e: unknown, prefix: 'config.' | 'rule.'): Verdict | null {
  if (!(e instanceof ApiError) || e.status !== 400) return null
  const field = e.details?.field
  if (typeof field !== 'string' || !field.startsWith(prefix)) return null
  return invalidVerdict(e.message, field.slice(prefix.length))
}

export default function OrgCustomRulesSection({orgId, isAdmin}: {orgId: string; isAdmin: boolean}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const {confirm} = useConfirm()
  const [draft, setDraft] = useState<{rule?: OrgCustomRule; config: OrgCustomRuleConfig} | null>(null)

  const {data, isLoading, error, refetch} = useAsync<OrgCustomRule[]>(() => orgRulesApi.list(orgId), [orgId])
  const rules = useMemo(() => data ?? [], [data])

  const handleDelete = useCallback(
    async (rule: OrgCustomRule) => {
      const ok = await confirm({
        title: t('customRules.deleteTitle', {name: rule.config.name || rule.ruleId}),
        message: t('customRules.deleteMessage'),
        confirmLabel: t('customRules.deleteConfirm'),
        danger: true,
      })
      if (!ok) return
      try {
        await orgRulesApi.remove(orgId, rule.id)
        toast.success(t('customRules.deleted', {name: rule.config.name || rule.ruleId}))
        refetch()
      } catch (e) {
        toast.error(t('customRules.deleteFailed'), {description: getErrorMessage(e)})
      }
    },
    [confirm, orgId, refetch, t, toast],
  )

  const handleToggle = useCallback(
    async (rule: OrgCustomRule) => {
      try {
        await orgRulesApi.setEnabled(orgId, rule.id, !rule.enabled, rule.config)
        refetch()
      } catch (e) {
        toast.error(t('customRules.updateFailed'), {description: getErrorMessage(e)})
      }
    },
    [orgId, refetch, t, toast],
  )

  if (isLoading) return <div className="text-2xs text-text-tertiary">{t('customRules.loading')}</div>
  if (error)
    return <div className="text-2xs text-semantic-error">{t('customRules.loadFailed', {message: String(error)})}</div>

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <div>
          <h4 className="text-sm font-medium">{t('customRules.title')}</h4>
          <p className="text-2xs text-text-tertiary">{t('customRules.subtitle')}</p>
        </div>
        {isAdmin && (
          <Button size="sm" onClick={() => setDraft({config: emptyRule()})} disabled={!!draft}>
            <Plus className="size-3.5" /> {t('customRules.newRule')}
          </Button>
        )}
      </div>

      {rules.length === 0 && !draft && (
        <p className="text-2xs text-text-tertiary">
          {t('customRules.none')}
          {isAdmin ? ` ${t('customRules.noneAdminHint')}` : ''}
        </p>
      )}

      <ul className="space-y-2">
        {rules.map(rule => {
          const newer = isNewerRule(rule)
          return (
            <li
              key={rule.id}
              className={clsx('rounded border border-border-subtle p-2.5', !rule.enabled && 'opacity-60')}
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="truncate text-sm font-medium">{rule.config.name || rule.ruleId}</span>
                    <span
                      className={clsx(
                        'rounded border px-1.5 py-0.5 text-2xs uppercase',
                        severityTone(rule.config.severity).text,
                        severityTone(rule.config.severity).bg,
                        severityTone(rule.config.severity).border,
                      )}
                    >
                      {rule.config.severity}
                    </span>
                  </div>
                  <div className="mt-0.5 truncate text-2xs text-text-tertiary">
                    <code>{rule.ruleId}</code>
                    {rule.config.rawTypeMatch
                      ? ` · ${t('customRules.matchesAction', {pattern: rule.config.rawTypeMatch})}`
                      : ''}
                    {rule.config.nameMatch
                      ? ` · ${t('customRules.matchesName', {pattern: rule.config.nameMatch})}`
                      : ''}
                  </div>
                  {rule.problem && (
                    <p className="mt-1 flex items-start gap-1 text-2xs text-semantic-warning">
                      <AlertTriangle className="mt-px size-3 shrink-0" aria-hidden />
                      <span>
                        {newer ? t('customRules.problemNewer') : t('customRules.problem', {problem: rule.problem})}
                      </span>
                    </p>
                  )}
                </div>
                {isAdmin && (
                  <div className="flex shrink-0 items-center gap-1">
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={rule.enabled ? t('customRules.pauseAria') : t('customRules.enableAria')}
                      title={rule.enabled ? t('customRules.pauseTitle') : t('customRules.enableTitle')}
                      onClick={() => handleToggle(rule)}
                    >
                      {rule.enabled ? <ShieldCheck className="size-3.5" /> : <ShieldOff className="size-3.5" />}
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t('customRules.editAria')}
                      title={newer ? t('customRules.editNewerTitle') : undefined}
                      disabled={newer}
                      onClick={() => setDraft({rule, config: rule.config})}
                    >
                      <Pencil className="size-3.5" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t('customRules.deleteAria')}
                      onClick={() => handleDelete(rule)}
                    >
                      <Trash2 className="size-3.5 text-semantic-error" />
                    </Button>
                  </div>
                )}
              </div>
            </li>
          )
        })}
      </ul>

      {draft && (
        <RuleEditor
          key={draft.rule?.id ?? 'new'}
          orgId={orgId}
          initial={draft.config}
          saved={draft.rule}
          takenIds={rules.map(r => r.ruleId)}
          onCancel={() => setDraft(null)}
          onSaved={() => {
            setDraft(null)
            refetch()
          }}
        />
      )}
    </div>
  )
}

function RuleEditor({
  orgId,
  initial,
  saved,
  takenIds,
  onCancel,
  onSaved,
}: {
  orgId: string
  initial: OrgCustomRuleConfig
  saved?: OrgCustomRule
  takenIds?: string[]
  onCancel: () => void
  onSaved: () => void
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const uid = useId()
  const editing = !!saved
  const [cfg, setCfg] = useState<OrgCustomRuleConfig>(initial)
  const [verdict, setVerdict] = useState<Verdict | null>(saved?.problem ? invalidVerdict(saved.problem) : null)
  const [saving, setSaving] = useState(false)
  const [testFlowId, setTestFlowId] = useState('')
  const [testResult, setTestResult] = useState<{matches: number; flowName: string} | null>(null)
  const [testing, setTesting] = useState(false)

  const {data: flows} = useAsync(() => libraryApi.list({orgId, limit: 50}).then(r => r.items ?? []), [orgId])

  const set = <K extends keyof OrgCustomRuleConfig>(key: K, value: OrgCustomRuleConfig[K]) => {
    setCfg(prev => ({...prev, [key]: value}))
    setVerdict(null)
    setTestResult(null)
  }

  const validate = useCallback(async () => {
    try {
      const res = await analysisApi.validateCustomRules([cfg])
      const v = verdictOf(res?.entries?.[0], editing)
      setVerdict(v)
      return v.valid
    } catch (e) {
      logger.error('custom rule validation failed', e)
      setVerdict(invalidVerdict(t('customRules.validatorUnreachable')))
      return false
    }
  }, [cfg, editing, t])

  const tryRule = useCallback(async () => {
    if (!testFlowId) return
    setTesting(true)
    try {
      const res = await analysisApi.testCustomRule(cfg, testFlowId)
      setTestResult({matches: res.matches, flowName: res.flowName})
    } catch (e) {
      setVerdict(fieldErrorOf(e, 'rule.') ?? invalidVerdict(getErrorMessage(e)))
      setTestResult(null)
    } finally {
      setTesting(false)
    }
  }, [cfg, testFlowId])

  const save = useCallback(async () => {
    if (!editing && takenIds?.includes(cfg.id.trim())) {
      setVerdict({valid: false, fieldErrors: {id: t('customRules.idTaken')}, errors: [], warnings: []})
      return
    }
    if (!(await validate())) return
    setSaving(true)
    try {
      await orgRulesApi.save(orgId, cfg, saved?.enabled ?? true, saved?.id)
      toast.success(t('customRules.saved', {name: cfg.name || cfg.id}))
      onSaved()
    } catch (e) {
      const fieldError = fieldErrorOf(e, 'config.')
      if (fieldError) setVerdict(fieldError)
      toast.error(t('customRules.saveFailed'), {description: getErrorMessage(e)})
    } finally {
      setSaving(false)
    }
  }, [cfg, editing, onSaved, orgId, saved, takenIds, t, toast, validate])

  const fieldError = (key: FormField, id: string) =>
    verdict?.fieldErrors[key] ? (
      <span id={id} className="mt-0.5 block text-2xs text-semantic-error">
        {verdict.fieldErrors[key]}
      </span>
    ) : null

  const field = (label: string, key: FormField, placeholder: string, hint?: string, readOnly = false) => {
    const errorId = `${uid}-${key}-error`
    const invalid = !!verdict?.fieldErrors[key]
    return (
      <label className="block">
        <span className="text-2xs text-text-tertiary">{label}</span>
        <input
          className={clsx(
            'mt-0.5 w-full rounded border bg-surface-1 px-2 py-1 text-sm',
            invalid ? 'border-semantic-error' : 'border-border-subtle',
            readOnly && 'cursor-not-allowed text-text-secondary',
          )}
          value={(cfg[key] as string) ?? ''}
          placeholder={placeholder}
          readOnly={readOnly}
          aria-invalid={invalid || undefined}
          aria-describedby={invalid ? errorId : undefined}
          onChange={e => set(key, e.target.value as OrgCustomRuleConfig[typeof key])}
        />
        {hint && <span className="mt-0.5 block text-2xs text-text-tertiary">{hint}</span>}
        {fieldError(key, errorId)}
      </label>
    )
  }

  const otherErrors = verdict && !verdict.valid ? verdict.errors : []
  const unexplained =
    !!verdict && !verdict.valid && otherErrors.length === 0 && !Object.keys(verdict.fieldErrors).length

  return (
    <div className="space-y-2.5 rounded border border-border-subtle bg-surface-2 p-3">
      <div className="grid gap-2.5 sm:grid-cols-2">
        {field(
          t('customRules.fieldId'),
          'id',
          t('customRules.fieldIdPlaceholder'),
          editing ? t('customRules.fieldIdLocked') : t('customRules.fieldIdHint'),
          editing,
        )}
        {field(t('customRules.fieldName'), 'name', t('customRules.fieldNamePlaceholder'))}
      </div>
      {field(t('customRules.fieldDescription'), 'description', t('customRules.fieldDescriptionPlaceholder'))}
      <div className="grid gap-2.5 sm:grid-cols-2">
        {field(
          t('customRules.fieldRawType'),
          'rawTypeMatch',
          t('customRules.fieldRawTypePlaceholder'),
          t('customRules.regexHint'),
        )}
        {field(
          t('customRules.fieldNameMatch'),
          'nameMatch',
          t('customRules.fieldNameMatchPlaceholder'),
          t('customRules.regexHint'),
        )}
      </div>
      {field(t('customRules.fieldSuggestion'), 'suggestion', t('customRules.fieldSuggestionPlaceholder'))}

      <div role="group" aria-label={t('customRules.severityLabel')}>
        <span className="text-2xs text-text-tertiary">{t('customRules.severityLabel')}</span>
        <div className="mt-0.5 flex gap-1.5">
          {SEVERITIES.map(sev => (
            <button
              key={sev}
              type="button"
              aria-pressed={cfg.severity === sev}
              onClick={() => set('severity', sev)}
              className={clsx(
                'rounded px-2 py-1 text-2xs uppercase',
                cfg.severity === sev
                  ? clsx(severityTone(sev).text, severityTone(sev).bg, 'border', severityTone(sev).border)
                  : 'text-text-tertiary hover:bg-surface-3',
              )}
            >
              {sev}
            </button>
          ))}
        </div>
        {fieldError('severity', `${uid}-severity-error`)}
      </div>

      {(otherErrors.length > 0 || unexplained) && (
        <div
          role="alert"
          className="rounded border border-semantic-error/40 bg-semantic-error/10 p-2 text-2xs text-semantic-error"
        >
          {otherErrors.length > 0 ? otherErrors.join('; ') : t('customRules.invalid')}
        </div>
      )}
      {verdict?.valid && <div className="text-2xs text-semantic-success">{t('customRules.valid')}</div>}
      {verdict?.warnings.map(w => (
        <p key={w} className="text-2xs text-semantic-warning">
          {t('customRules.warning', {message: w})}
        </p>
      ))}

      {(flows?.length ?? 0) > 0 && (
        <div className="flex flex-wrap items-end gap-2 border-t border-border-subtle pt-2.5">
          <label className="min-w-0 flex-1">
            <span className="text-2xs text-text-tertiary">{t('customRules.tryOn')}</span>
            <select
              className="mt-0.5 w-full rounded border border-border-subtle bg-surface-1 px-2 py-1 text-sm"
              value={testFlowId}
              onChange={e => {
                setTestFlowId(e.target.value)
                setTestResult(null)
              }}
            >
              <option value="">{t('customRules.tryPick')}</option>
              {(flows ?? []).map(f => (
                <option key={f.id} value={f.id}>
                  {f.name}
                </option>
              ))}
            </select>
          </label>
          <Button size="sm" variant="secondary" onClick={tryRule} disabled={!testFlowId || testing}>
            {testing ? t('customRules.trying') : t('customRules.tryIt')}
          </Button>
          {testResult && (
            <p
              className={clsx('text-2xs', testResult.matches > 0 ? 'text-semantic-success' : 'text-semantic-warning')}
              role="status"
            >
              {testResult.matches > 0
                ? t('customRules.tryMatches', {count: testResult.matches, flow: testResult.flowName})
                : t('customRules.tryNoMatches', {flow: testResult.flowName})}
            </p>
          )}
        </div>
      )}

      <div className="flex justify-end gap-2">
        <Button size="sm" variant="ghost" onClick={onCancel}>
          {t('customRules.cancel')}
        </Button>
        <Button size="sm" variant="secondary" onClick={validate}>
          {t('customRules.check')}
        </Button>
        <Button size="sm" onClick={save} disabled={saving || !cfg.id}>
          {saving ? t('customRules.saving') : t('customRules.save')}
        </Button>
      </div>
    </div>
  )
}
