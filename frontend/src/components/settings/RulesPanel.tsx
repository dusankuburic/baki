import {useCallback, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {analysisApi} from '@/api'
import type {Rule, RuleConfig, Severity} from '@/types'
import {Switch, NumberField} from '@/components/shared'
import {AlertCircle, AlertTriangle, Info, Zap} from 'lucide-react'
import SegmentedControl from '@/components/shared/SegmentedControl'
import {useSettingsStore} from '@/stores/settingsStore'
import {useAuthStore, useOrgStore} from '@/stores'
import {settingsApi} from '@/api'
import {useToast} from '@/components/shared/Toast'
import {useAsync} from '@/hooks/useAsync'
import PreviewBadge from '@/components/analyzer/PreviewBadge'
import clsx from 'clsx'

const SCOPE_DEPLOYMENT = '__deployment__'

export default function RulesPanel() {
  const {t} = useTranslation('settings')
  const settings = useSettingsStore(s => s.settings)
  const updateSettings = useSettingsStore(s => s.updateSettings)
  const autoAnalyzeOnOpen = settings.analysis.autoAnalyzeOnOpen
  const toast = useToast()

  const currentUser = useAuthStore(s => s.user)
  const organisations = useOrgStore(s => s.organisations)

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

  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const canEdit = !isAuthenticated || (isOrgScope ? adminOrgs.some(o => o.id === scope) : isSystemAdmin)

  const {
    data,
    isLoading: loading,
    error: loadError,
    setData: setRules,
  } = useAsync<Rule[]>(() => analysisApi.getRules().then(res => res || []), [])
  const rules = data ?? []

  const {data: orgOverrides, refetch: refetchOverrides} = useAsync<Record<string, RuleConfig>>(async () => {
    if (!isOrgScope) return {}
    const s = await settingsApi.getOrgSettings(scope)
    return (s?.analysis?.rules as Record<string, RuleConfig> | undefined) ?? {}
  }, [scope, isOrgScope])
  const overrides = useMemo(() => orgOverrides ?? {}, [orgOverrides])

  const effective = useCallback(
    (rule: Rule): {enabled: boolean; severity: Severity; overridden: boolean; options?: Record<string, number>} => {
      const ov = isOrgScope ? overrides[rule.id] : undefined
      if (!ov) return {enabled: rule.enabled, severity: rule.defaultSeverity, overridden: false, options: rule.options}
      return {
        enabled: ov.enabled,
        severity: (ov.severity || rule.defaultSeverity) as Severity,
        overridden: true,
        options: {...(rule.options ?? {}), ...((ov.options as Record<string, number> | undefined) ?? {})},
      }
    },
    [isOrgScope, overrides],
  )

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

  const handleOptionChange = async (ruleId: string, key: string, n: number) => {
    const rule = rules.find(r => r.id === ruleId)
    if (!rule) return
    const eff = effective(rule)
    const options = {...(eff.options ?? {}), [key]: n}
    try {
      if (isOrgScope) {
        await writeOrgOverride(ruleId, {enabled: eff.enabled, severity: eff.severity, options})
        return
      }
      await analysisApi.updateRuleConfig(ruleId, {enabled: rule.enabled, severity: rule.defaultSeverity, options})
      setRules(rules.map(r => (r.id === ruleId ? {...r, options} : r)))
    } catch (err) {
      toast.error(t('rules.updateFailed', {message: (err as Error).message}))
    }
  }

  if (loading) return <div className="p-8 text-center text-text-tertiary">{t('rules.loading')}</div>

  if (loadError)
    return (
      <div className="space-y-6">
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
      {(isSystemAdmin || adminOrgs.length > 0) && (
        <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-2">
          <label htmlFor="rules-scope" className="text-sm font-bold text-text-primary">
            {t('rules.appliesTo')}
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
            {t(isOrgScope ? 'rules.scopeOrgHint' : 'rules.scopeDeploymentHint')}
          </p>
          {!canEdit && (
            <p role="status" className="text-xs text-semantic-warning">
              {t(isOrgScope ? 'rules.scopeReadOnlyOrg' : 'rules.scopeReadOnlyDeployment')}
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
                i !== rules.length - 1 && 'border-b border-border-subtle',
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
                    {rule.stage === 'preview' && <PreviewBadge of="rule" />}
                    {rule.kind === 'library' && (
                      <span
                        className="text-2xs font-black uppercase tracking-widest px-1.5 py-0.5 rounded bg-surface-3 text-text-tertiary"
                        title={t('rules.libraryKindHint')}
                      >
                        {t('rules.libraryKind')}
                      </span>
                    )}
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
                    activation="manual"
                    options={[
                      {value: 'error', label: t('rules.severity.error'), icon: AlertCircle},
                      {value: 'warning', label: t('rules.severity.warning'), icon: AlertTriangle},
                      {value: 'info', label: t('rules.severity.info'), icon: Info},
                    ]}
                    className="bg-surface-2"
                  />
                </div>
              )}

              {eff.enabled && eff.options && Object.keys(eff.options).length > 0 && (
                <div
                  className="flex flex-wrap items-center gap-4 animate-fade-in"
                  data-testid={`thresholds-${rule.id}`}
                >
                  <span className="text-2xs font-bold uppercase text-text-tertiary">{t('rules.thresholds')}</span>
                  {Object.entries(eff.options).map(([key, val]) => (
                    <label key={key} className="flex items-center gap-2 text-xs text-text-secondary">
                      <span className="font-mono">{key}</span>
                      {canEdit ? (
                        <div className="w-20">
                          <NumberField
                            value={Number(val)}
                            min={1}
                            fallback={Number(rule.options?.[key] ?? 1)}
                            onCommit={n => void handleOptionChange(rule.id, key, n)}
                          />
                        </div>
                      ) : (
                        <span className="font-mono text-text-primary">{String(val)}</span>
                      )}
                    </label>
                  ))}
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
