import {useCallback, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {analysisApi} from '@/api'
import type {Rule, RuleConfig, Severity} from '@/types'
import {Switch} from '@/components/shared'
import {Shield, AlertCircle, AlertTriangle, Info, Zap} from 'lucide-react'
import SegmentedControl from '@/components/shared/SegmentedControl'
import {useSettingsStore} from '@/stores/settingsStore'
import {useAuthStore, useOrgStore} from '@/stores'
import {settingsApi} from '@/api'
import {useToast} from '@/components/shared/Toast'
import {useAsync} from '@/hooks/useAsync'
import clsx from 'clsx'

// SCOPE_DEPLOYMENT is the sentinel for "the whole deployment" in the scope
// selector — distinct from an org id, which is a UUID.
const SCOPE_DEPLOYMENT = '__deployment__'

export default function RulesPanel() {
  const {t} = useTranslation('settings')
  const settings = useSettingsStore(s => s.settings)
  const updateSettings = useSettingsStore(s => s.updateSettings)
  const autoAnalyzeOnOpen = settings.analysis.autoAnalyzeOnOpen
  const toast = useToast()

  const currentUser = useAuthStore(s => s.user)
  const organisations = useOrgStore(s => s.organisations)

  // Rule configuration is now SCOPED (R4). Deployment settings apply to every
  // tenant and are system-admin only; an org's profile applies to that org's
  // flows and is editable by its admins. Before R4 this panel wrote the
  // deployment singleton from any member's session, so one team's toggle
  // silently changed analysis for everyone.
  const adminOrgs = useMemo(
    () =>
      organisations.filter(
        o => o.ownerId === currentUser?.id || o.members.some(m => m.userId === currentUser?.id && m.role === 'admin'),
      ),
    [organisations, currentUser?.id],
  )
  const isSystemAdmin = currentUser?.role === 'admin'
  const [scope, setScope] = useState<string>(() =>
    isSystemAdmin ? SCOPE_DEPLOYMENT : (adminOrgs[0]?.id ?? SCOPE_DEPLOYMENT),
  )
  const isOrgScope = scope !== SCOPE_DEPLOYMENT

  // Mirrors the backend's gate exactly, including its local-mode carve-out:
  // SecurityConfig.RequireRole returns true immediately when JWT auth is
  // disabled, because desktop/local is a single user with no tenants. Without
  // this first clause the panel would render read-only for every desktop user —
  // a regression the multi-tenancy reasoning does not apply to at all.
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const canEdit = !isAuthenticated || (isOrgScope ? adminOrgs.some(o => o.id === scope) : isSystemAdmin)

  const {
    data,
    isLoading: loading,
    error: loadError,
    setData: setRules,
  } = useAsync<Rule[]>(() => analysisApi.getRules().then(res => res || []), [])
  const rules = data ?? []

  // The selected org's overrides, keyed by rule id. Absent = inherited from the
  // deployment layer, which the resolver merges PER RULE — so showing which
  // rules an org has actually changed is the difference between "we configured
  // this" and "this is just the default".
  const {data: orgOverrides, refetch: refetchOverrides} = useAsync<Record<string, RuleConfig>>(async () => {
    if (!isOrgScope) return {}
    const s = await settingsApi.getOrgSettings(scope)
    return (s?.analysis?.rules as Record<string, RuleConfig> | undefined) ?? {}
  }, [scope, isOrgScope])
  // Memoized: `orgOverrides ?? {}` mints a fresh object every render, which
  // would change the identity of every callback depending on it.
  const overrides = useMemo(() => orgOverrides ?? {}, [orgOverrides])

  // Effective config for a rule under the current scope: the org's override
  // when it has one, otherwise what the server reported (the deployment layer).
  const effective = useCallback(
    (rule: Rule): {enabled: boolean; severity: Severity; overridden: boolean} => {
      const ov = isOrgScope ? overrides[rule.id] : undefined
      if (!ov) return {enabled: rule.enabled, severity: rule.defaultSeverity, overridden: false}
      return {
        enabled: ov.enabled,
        severity: (ov.severity || rule.defaultSeverity) as Severity,
        overridden: true,
      }
    },
    [isOrgScope, overrides],
  )

  // Writing an ORG profile is a merge, not a replace: sending only the changed
  // rule would drop every other override the org has set.
  const writeOrgOverride = useCallback(
    async (ruleId: string, patch: RuleConfig) => {
      const merged = {...overrides, [ruleId]: patch}
      await settingsApi.updateOrgSettings(scope, {analysis: {rules: merged}} as never)
      refetchOverrides()
    },
    [overrides, refetchOverrides, scope],
  )

  const handleToggle = async (ruleId: string, enabled: boolean) => {
    try {
      if (isOrgScope) {
        const rule = rules.find(r => r.id === ruleId)
        const cur = rule ? effective(rule) : {severity: 'warning' as Severity}
        await writeOrgOverride(ruleId, {enabled, severity: cur.severity})
        return
      }
      // Dedicated toggle endpoint: preserves the configured severity override
      // and option thresholds, unlike a full-config replace.
      await analysisApi.setRuleEnabled(ruleId, enabled)
      setRules(rules.map(r => (r.id === ruleId ? {...r, enabled} : r)))
    } catch (err) {
      toast.error(t('rules.updateFailed', {message: (err as Error).message}))
    }
  }

  const handleSeverityChange = async (ruleId: string, severity: Severity) => {
    const rule = rules.find(r => r.id === ruleId)
    if (!rule) return

    try {
      if (isOrgScope) {
        await writeOrgOverride(ruleId, {enabled: effective(rule).enabled, severity})
        return
      }
      const config: RuleConfig = {
        enabled: rule.enabled,
        severity: severity,
      }
      await analysisApi.updateRuleConfig(ruleId, config)
      setRules(rules.map(r => (r.id === ruleId ? {...r, defaultSeverity: severity} : r)))
    } catch (err) {
      toast.error(t('rules.severityFailed', {message: (err as Error).message}))
    }
  }

  if (loading) return <div className="p-8 text-center text-text-tertiary">{t('rules.loading')}</div>

  if (loadError)
    return (
      <div className="space-y-6">
        <div>
          <h2 className="text-xl font-semibold text-text-primary flex items-center gap-2">
            <Shield size={20} className="text-brand-500" />
            {t('rules.title')}
          </h2>
        </div>
        <div className="p-4 flex items-start gap-3 border border-red-500/30 bg-red-500/5 rounded-xl">
          <AlertCircle className="text-red-400 shrink-0 mt-0.5" size={18} />
          <div>
            <p className="text-sm font-medium text-red-400">{t('rules.loadFailed')}</p>
            <p className="text-xs text-text-tertiary mt-1">{loadError}</p>
          </div>
        </div>
      </div>
    )

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-xl font-semibold text-text-primary flex items-center gap-2">
          <Shield size={20} className="text-brand-500" />
          Analysis Rules
        </h2>
        <p className="text-sm text-text-secondary mt-1">{t('rules.subtitle')}</p>
      </div>

      {(isSystemAdmin || adminOrgs.length > 0) && (
        <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-2">
          <label htmlFor="rules-scope" className="text-sm font-bold text-text-primary">
            Applies to
          </label>
          <select
            id="rules-scope"
            className="w-full rounded-lg border border-border-default bg-surface-2 px-2 py-1.5 text-sm"
            value={scope}
            onChange={e => setScope(e.target.value)}
          >
            {isSystemAdmin && <option value={SCOPE_DEPLOYMENT}>{t('rules.scopeDeployment')}</option>}
            {adminOrgs.map(o => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </select>
          <p className="text-xs text-text-tertiary">
            {isOrgScope
              ? "Changes apply to this organisation's flows only. Rules you don't change stay at the deployment default."
              : 'Changes apply to every organisation in this deployment.'}
          </p>
          {!canEdit && (
            <p role="status" className="text-xs text-semantic-warning">
              You can view this configuration but not change it — that needs
              {isOrgScope ? ' admin of this organisation.' : ' a system administrator.'}
            </p>
          )}
        </div>
      )}

      <div className="p-4 flex items-center justify-between border border-border-default rounded-xl bg-surface-1">
        <div className="flex items-start gap-3">
          <Zap size={18} className="text-brand-500 shrink-0 mt-0.5" />
          <div>
            <h3 className="text-sm font-bold text-text-primary">{t('rules.autoAnalyzeTitle')}</h3>
            <p className="text-xs text-text-tertiary mt-1">{t('rules.autoAnalyzeHint')}</p>
          </div>
        </div>
        <Switch
          checked={autoAnalyzeOnOpen}
          onChange={v => updateSettings({analysis: {...settings.analysis, autoAnalyzeOnOpen: v}})}
        />
      </div>

      <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
        {rules.map((rule, i) => {
          const eff = effective(rule)
          return (
            <div
              key={rule.id}
              className={clsx(
                'p-4 flex flex-col gap-3 transition-colors',
                i !== rules.length - 1 && 'border-bottom border-border-subtle',
                !eff.enabled && 'opacity-60 bg-surface-2/30',
              )}
            >
              <div className="flex items-center justify-between">
                <div className="flex-1">
                  <div className="flex items-center gap-2">
                    <h3 className="text-sm font-bold text-text-primary">{rule.name}</h3>
                    <span className="text-2xs font-black uppercase tracking-widest px-1.5 py-0.5 rounded bg-surface-3 text-text-tertiary">
                      {rule.category}
                    </span>
                    {/* An org admin needs to tell "we decided this" from "this is
                      just the default" — the resolver merges per rule, so most
                      rules in an org scope are inherited. */}
                    {isOrgScope && eff.overridden && (
                      <span className="text-2xs font-black uppercase tracking-widest px-1.5 py-0.5 rounded bg-brand-500/15 text-brand-500">
                        {t('rules.overridden')}
                      </span>
                    )}
                  </div>
                  <p className="text-xs text-text-tertiary mt-1">{rule.description}</p>
                </div>
                <Switch
                  checked={eff.enabled}
                  disabled={!canEdit}
                  onChange={checked => handleToggle(rule.id, checked)}
                />
              </div>

              {eff.enabled && (
                <div className="flex items-center gap-4 animate-fade-in" title={t('rules.severityTooltip')}>
                  <span className="text-2xs font-bold uppercase text-text-tertiary">{t('rules.reportAs')}</span>
                  <SegmentedControl
                    size="sm"
                    value={eff.severity}
                    disabled={!canEdit}
                    onChange={v => handleSeverityChange(rule.id, v as Severity)}
                    options={[
                      {value: 'error', label: t('rules.severity.error'), icon: AlertCircle},
                      {value: 'warning', label: t('rules.severity.warning'), icon: AlertTriangle},
                      {value: 'info', label: t('rules.severity.info'), icon: Info},
                    ]}
                    className="bg-surface-2"
                  />
                </div>
              )}
            </div>
          )
        })}
      </div>

      <div className="p-4 bg-brand-500/5 border border-brand-500/10 rounded-lg flex gap-3">
        <Info className="text-brand-500 shrink-0" size={18} />
        <p className="text-xs text-text-secondary leading-relaxed">
          {t('rules.tipPrefix')} <code className="text-brand-400">missing-delay</code> {t('rules.tipSuffix')}
        </p>
      </div>
    </div>
  )
}
