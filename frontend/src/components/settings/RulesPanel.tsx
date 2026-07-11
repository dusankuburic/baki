import {analysisApi} from '@/api'
import type {Rule, RuleConfig, Severity} from '@/types'
import {Switch} from '@/components/shared'
import {Shield, AlertCircle, AlertTriangle, Info, Zap} from 'lucide-react'
import SegmentedControl from '@/components/shared/SegmentedControl'
import {useSettingsStore} from '@/stores/settingsStore'
import {useToast} from '@/components/shared/Toast'
import {useAsync} from '@/hooks/useAsync'
import clsx from 'clsx'

export default function RulesPanel() {
  const settings = useSettingsStore(s => s.settings)
  const updateSettings = useSettingsStore(s => s.updateSettings)
  const autoAnalyzeOnOpen = settings.analysis.autoAnalyzeOnOpen
  const toast = useToast()

  const {data, isLoading: loading, error: loadError, setData: setRules} = useAsync<Rule[]>(
    () => analysisApi.getRules().then(res => res || []),
    [],
  )
  const rules = data ?? []

  const handleToggle = async (ruleId: string, enabled: boolean) => {
    try {
      // Dedicated toggle endpoint: preserves the configured severity override
      // and option thresholds, unlike a full-config replace.
      await analysisApi.setRuleEnabled(ruleId, enabled)
      setRules(rules.map(r => r.id === ruleId ? {...r, enabled} : r))
    } catch (err) {
      toast.error('Failed to update rule: ' + (err as Error).message)
    }
  }

  const handleSeverityChange = async (ruleId: string, severity: Severity) => {
    const rule = rules.find(r => r.id === ruleId)
    if (!rule) return

    try {
      const config: RuleConfig = {
        enabled: rule.enabled,
        severity: severity
      }
      await analysisApi.updateRuleConfig(ruleId, config)
      setRules(rules.map(r => r.id === ruleId ? {...r, defaultSeverity: severity} : r))
    } catch (err) {
      toast.error('Failed to update rule severity: ' + (err as Error).message)
    }
  }

  if (loading) return <div className="p-8 text-center text-text-tertiary">Loading rules...</div>

  if (loadError) return (
    <div className="space-y-6">
      <div>
        <h2 className="text-xl font-semibold text-text-primary flex items-center gap-2">
          <Shield size={20} className="text-brand-500" />
          Analysis Rules
        </h2>
      </div>
      <div className="p-4 flex items-start gap-3 border border-red-500/30 bg-red-500/5 rounded-xl">
        <AlertCircle className="text-red-400 shrink-0 mt-0.5" size={18} />
        <div>
          <p className="text-sm font-medium text-red-400">Failed to load rules</p>
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
        <p className="text-sm text-text-secondary mt-1">
          Configure which static analysis rules are active and their reporting severity.
        </p>
      </div>

      <div className="p-4 flex items-center justify-between border border-border-default rounded-xl bg-surface-1">
        <div className="flex items-start gap-3">
          <Zap size={18} className="text-brand-500 shrink-0 mt-0.5" />
          <div>
            <h3 className="text-sm font-bold text-text-primary">Auto-analyze on flow open</h3>
            <p className="text-xs text-text-tertiary mt-1">
              Run all enabled rules automatically as soon as a flow finishes loading.
            </p>
          </div>
        </div>
        <Switch
          checked={autoAnalyzeOnOpen}
          onChange={(v) => updateSettings({analysis: {...settings.analysis, autoAnalyzeOnOpen: v}})}
        />
      </div>

      <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
        {rules.map((rule, i) => (
          <div 
            key={rule.id} 
            className={clsx(
              "p-4 flex flex-col gap-3 transition-colors",
              i !== rules.length - 1 && "border-bottom border-border-subtle",
              !rule.enabled && "opacity-60 bg-surface-2/30"
            )}
          >
            <div className="flex items-center justify-between">
              <div className="flex-1">
                <div className="flex items-center gap-2">
                  <h3 className="text-sm font-bold text-text-primary">{rule.name}</h3>
                  <span className="text-2xs font-black uppercase tracking-widest px-1.5 py-0.5 rounded bg-surface-3 text-text-tertiary">
                    {rule.category}
                  </span>
                </div>
                <p className="text-xs text-text-tertiary mt-1">{rule.description}</p>
              </div>
              <Switch 
                checked={rule.enabled} 
                onChange={(checked) => handleToggle(rule.id, checked)} 
              />
            </div>

            {rule.enabled && (
              <div className="flex items-center gap-4 animate-fade-in" title="Severity changes apply to the next analysis run">
                <span className="text-2xs font-bold uppercase text-text-tertiary">Report as:</span>
                <SegmentedControl
                  size="sm"
                  value={rule.defaultSeverity}
                  onChange={(v) => handleSeverityChange(rule.id, v as Severity)}
                  options={[
                    {value: 'error', label: 'Error', icon: AlertCircle},
                    {value: 'warning', label: 'Warning', icon: AlertTriangle},
                    {value: 'info', label: 'Info', icon: Info},
                  ]}
                  className="bg-surface-2"
                />
              </div>
            )}
          </div>
        ))}
      </div>
      
      <div className="p-4 bg-brand-500/5 border border-brand-500/10 rounded-lg flex gap-3">
        <Info className="text-brand-500 shrink-0" size={18} />
        <p className="text-xs text-text-secondary leading-relaxed">
          <strong>Tip:</strong> Elevate rules like <code className="text-brand-400">missing-delay</code> to <strong>Error</strong> when debugging UI synchronization issues to make them stand out in the findings list. Severity changes apply to the <strong>next</strong> analysis run.
        </p>
      </div>
    </div>
  )
}
