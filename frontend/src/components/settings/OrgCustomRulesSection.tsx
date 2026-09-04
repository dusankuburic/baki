import {useCallback, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Plus, Trash2, Pencil, ShieldCheck, ShieldOff} from 'lucide-react'
import {orgRulesApi, type OrgCustomRule, type OrgCustomRuleConfig} from '@/api/governance'
import {analysisApi, libraryApi} from '@/api'
import {Button, useToast, useConfirm} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {logger} from '@/lib/logger'
import {severityTone} from '@/lib/severityTone'
import clsx from 'clsx'

// OrgCustomRulesSection manages an org's OWN analyzer rules (R4). Before it,
// custom rules came from a single deployment-wide JSON file that only someone
// with server access could change, so a team could not express its own policy
// ("flag any HTTP action without our approved retry") at all.
//
// Admin-only edits; members read — analysis configuration decides what a team is
// told about its own flows.

const SEVERITIES: OrgCustomRuleConfig['severity'][] = ['error', 'warning', 'info']

const emptyRule = (): OrgCustomRuleConfig => ({
  id: '',
  name: '',
  description: '',
  severity: 'warning',
  category: 'Style',
  rawTypeMatch: '',
})

export default function OrgCustomRulesSection({orgId, isAdmin}: {orgId: string; isAdmin: boolean}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const {confirm} = useConfirm()
  const [draft, setDraft] = useState<{config: OrgCustomRuleConfig; rowId?: string} | null>(null)

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
        toast.error(t('customRules.deleteFailed'), {description: String(e)})
      }
    },
    [confirm, orgId, refetch, t, toast],
  )

  const handleToggle = useCallback(
    async (rule: OrgCustomRule) => {
      try {
        await orgRulesApi.save(orgId, rule.config, !rule.enabled, rule.id)
        refetch()
      } catch (e) {
        toast.error(t('customRules.updateFailed'), {description: String(e)})
      }
    },
    [orgId, refetch, t, toast],
  )

  if (isLoading) return <div className="text-2xs text-muted">{t('customRules.loading')}</div>
  if (error) return <div className="text-2xs text-error">{t('customRules.loadFailed', {message: String(error)})}</div>

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <div>
          <h4 className="text-sm font-medium">{t('customRules.title')}</h4>
          <p className="text-2xs text-muted">{t('customRules.subtitle')}</p>
        </div>
        {isAdmin && (
          <Button size="sm" onClick={() => setDraft({config: emptyRule()})} disabled={!!draft}>
            <Plus className="size-3.5" /> {t('customRules.newRule')}
          </Button>
        )}
      </div>

      {rules.length === 0 && !draft && (
        <p className="text-2xs text-muted">
          {t('customRules.none')}
          {isAdmin ? ` ${t('customRules.noneAdminHint')}` : ''}
        </p>
      )}

      <ul className="space-y-2">
        {rules.map(rule => (
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
                <div className="mt-0.5 truncate text-2xs text-muted">
                  <code>{rule.ruleId}</code>
                  {rule.config.rawTypeMatch
                    ? ` · ${t('customRules.matchesAction', {pattern: rule.config.rawTypeMatch})}`
                    : ''}
                  {rule.config.nameMatch ? ` · ${t('customRules.matchesName', {pattern: rule.config.nameMatch})}` : ''}
                </div>
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
                    onClick={() => setDraft({config: rule.config, rowId: rule.id})}
                  >
                    <Pencil className="size-3.5" />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={t('customRules.deleteAria')}
                    onClick={() => handleDelete(rule)}
                  >
                    <Trash2 className="size-3.5 text-error" />
                  </Button>
                </div>
              )}
            </div>
          </li>
        ))}
      </ul>

      {draft && (
        <RuleEditor
          orgId={orgId}
          initial={draft.config}
          rowId={draft.rowId}
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

// RuleEditor is a FORM over CustomRuleConfig, not a JSON textarea: the whole
// point of the registry is that a team can author a rule without knowing the
// wire format. Validation runs against the server's own analyzer
// (POST /api/analysis/custom-rules/validate) so the preview, the save endpoint,
// and the analysis engine cannot disagree about whether a rule is valid.
function RuleEditor({
  orgId,
  initial,
  rowId,
  onCancel,
  onSaved,
}: {
  orgId: string
  initial: OrgCustomRuleConfig
  rowId?: string
  onCancel: () => void
  onSaved: () => void
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const [cfg, setCfg] = useState<OrgCustomRuleConfig>(initial)
  const [validation, setValidation] = useState<{valid: boolean; errors: string[]} | null>(null)
  const [saving, setSaving] = useState(false)
  // "Try it" state. Separate from validation because they answer different
  // questions: validation is "does this compile", this is "does it match
  // anything in a real flow".
  const [testFlowId, setTestFlowId] = useState('')
  const [testResult, setTestResult] = useState<{matches: number; flowName: string} | null>(null)
  const [testing, setTesting] = useState(false)

  // The caller's own flows, to pick a target to try the rule against.
  const {data: flows} = useAsync(() => libraryApi.list({orgId, limit: 50}).then(r => r.items ?? []), [orgId])

  const set = <K extends keyof OrgCustomRuleConfig>(key: K, value: OrgCustomRuleConfig[K]) => {
    setCfg(prev => ({...prev, [key]: value}))
    setValidation(null)
    // Editing invalidates the previous try — a stale "matches 3" next to a
    // changed regex is worse than no result at all.
    setTestResult(null)
  }

  const validate = useCallback(async () => {
    try {
      const res = await analysisApi.validateCustomRules([cfg])
      const first = res?.entries?.[0]
      const ok = !!first?.valid
      setValidation({valid: ok, errors: first?.error ? [first.error] : []})
      return ok
    } catch (e) {
      // A validation-endpoint failure must not silently look like "valid" —
      // that would let an unparseable rule reach the save and be rejected there
      // with a less specific message.
      logger.error('custom rule validation failed', e)
      setValidation({valid: false, errors: [t('customRules.validatorUnreachable')]})
      return false
    }
  }, [cfg, t])

  const tryRule = useCallback(async () => {
    if (!testFlowId) return
    setTesting(true)
    try {
      const res = await analysisApi.testCustomRule(cfg, testFlowId)
      setTestResult({matches: res.matches, flowName: res.flowName})
    } catch (e) {
      // Surface it as a validation failure: the usual cause is a rule that does
      // not compile, and the author needs the reason, not a silent nothing.
      setValidation({valid: false, errors: [String(e)]})
      setTestResult(null)
    } finally {
      setTesting(false)
    }
  }, [cfg, testFlowId])

  const save = useCallback(async () => {
    if (!(await validate())) return
    setSaving(true)
    try {
      await orgRulesApi.save(orgId, cfg, true, rowId)
      toast.success(t('customRules.saved', {name: cfg.name || cfg.id}))
      onSaved()
    } catch (e) {
      toast.error(t('customRules.saveFailed'), {description: String(e)})
    } finally {
      setSaving(false)
    }
  }, [cfg, onSaved, orgId, rowId, t, toast, validate])

  const field = (label: string, key: keyof OrgCustomRuleConfig, placeholder: string, hint?: string) => (
    <label className="block">
      <span className="text-2xs text-muted">{label}</span>
      <input
        className="mt-0.5 w-full rounded border border-border-subtle bg-surface px-2 py-1 text-sm"
        value={(cfg[key] as string) ?? ''}
        placeholder={placeholder}
        onChange={e => set(key, e.target.value as OrgCustomRuleConfig[typeof key])}
      />
      {hint && <span className="mt-0.5 block text-3xs text-muted">{hint}</span>}
    </label>
  )

  return (
    <div className="space-y-2.5 rounded border border-border-subtle bg-surface-raised p-3">
      <div className="grid gap-2.5 sm:grid-cols-2">
        {field(t('customRules.fieldId'), 'id', t('customRules.fieldIdPlaceholder'), t('customRules.fieldIdHint'))}
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

      <label className="block">
        <span className="text-2xs text-muted">{t('customRules.severityLabel')}</span>
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
      </label>

      {validation && !validation.valid && (
        <div role="alert" className="rounded border border-error/40 bg-error/10 p-2 text-2xs text-error">
          {validation.errors.length > 0 ? validation.errors.join('; ') : t('customRules.invalid')}
        </div>
      )}
      {validation?.valid && <div className="text-2xs text-success">{t('customRules.valid')}</div>}

      {/* Try it against a real flow. The validate call above only proves the
          rule COMPILES; a rule that compiles and matches nothing is the failure
          an author cannot otherwise see. */}
      {(flows?.length ?? 0) > 0 && (
        <div className="flex flex-wrap items-end gap-2 border-t border-border-subtle pt-2.5">
          <label className="min-w-0 flex-1">
            <span className="text-2xs text-muted">{t('customRules.tryOn')}</span>
            <select
              className="mt-0.5 w-full rounded border border-border-subtle bg-surface px-2 py-1 text-sm"
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
              className={clsx('text-2xs', testResult.matches > 0 ? 'text-success' : 'text-semantic-warning')}
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
