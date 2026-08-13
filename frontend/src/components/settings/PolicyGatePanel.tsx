import {useEffect, useState, useCallback} from 'react'
import {Plus, Trash2, Shield, ShieldCheck, ShieldAlert, ChevronDown, ChevronRight} from 'lucide-react'
import clsx from 'clsx'
import {analysisApi} from '@/api'
import type {Policy, PolicyRule, Severity, Rule} from '@/types'
import {useOrgStore} from '@/stores/orgStore'
import {useToast} from '@/components/shared/Toast'
import {useConfirm} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import SegmentedControl from '@/components/shared/SegmentedControl'

const GATE_OPTIONS: {label: string; value: Severity | ''}[] = [
  {label: 'Report only', value: ''},
  {label: 'Info+', value: 'info'},
  {label: 'Warning+', value: 'warning'},
  {label: 'Error+', value: 'error'},
]

export default function PolicyGatePanel() {
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const toast = useToast()
  const {confirm} = useConfirm()

  const {data: rules} = useAsync<Rule[]>(() => analysisApi.getRules().then(r => r || []), [])
  const ruleDefs = rules ?? []

  const [policies, setPolicies] = useState<Policy[]>([])
  const [loading, setLoading] = useState(false)
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)

  const loadPolicies = useCallback(async () => {
    if (!activeOrgId) return
    setLoading(true)
    try {
      const result = await analysisApi.listPolicies(activeOrgId)
      setPolicies(result || [])
    } catch {
      // 503 in local mode is expected
    } finally {
      setLoading(false)
    }
  }, [activeOrgId])

  useEffect(() => {
    void loadPolicies()
  }, [loadPolicies])

  const handleCreate = async () => {
    if (!activeOrgId) {
      toast.error('Select an organization first')
      return
    }
    const name = `Policy ${policies.length + 1}`
    const policy: Policy = {
      id: '',
      orgId: activeOrgId,
      name,
      description: '',
      rules: [],
      gateSeverity: 'warning',
    }
    try {
      const saved = await analysisApi.savePolicy(policy)
      setPolicies([...policies, saved])
      setExpandedId(saved.id)
      setCreating(false)
    } catch (err) {
      toast.error('Failed to create policy: ' + (err as Error).message)
    }
  }

  const handleDelete = async (id: string) => {
    if (!activeOrgId) return
    const ok = await confirm({
      title: 'Delete policy',
      message: 'This policy will be permanently deleted.',
      danger: true,
    })
    if (!ok) return
    try {
      await analysisApi.deletePolicy(activeOrgId, id)
      setPolicies(policies.filter(p => p.id !== id))
      if (expandedId === id) setExpandedId(null)
    } catch (err) {
      toast.error('Failed to delete policy: ' + (err as Error).message)
    }
  }

  const handleUpdate = async (policy: Policy) => {
    try {
      const saved = await analysisApi.savePolicy(policy)
      setPolicies(policies.map(p => (p.id === saved.id ? saved : p)))
    } catch (err) {
      toast.error('Failed to save policy: ' + (err as Error).message)
    }
  }

  const toggleRule = (policy: Policy, ruleId: string, enabled: boolean) => {
    const existing = policy.rules.find(r => r.ruleId === ruleId)
    let newRules: PolicyRule[]
    if (existing) {
      newRules = policy.rules.map(r => (r.ruleId === ruleId ? {...r, enabled} : r))
    } else {
      newRules = [...policy.rules, {ruleId, enabled}]
    }
    handleUpdate({...policy, rules: newRules})
  }

  const setRuleSeverity = (policy: Policy, ruleId: string, severity: Severity) => {
    const newRules = policy.rules.map(r =>
      r.ruleId === ruleId ? {...r, severity} : r,
    )
    handleUpdate({...policy, rules: newRules})
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-text-primary flex items-center gap-2">
            <Shield className="w-5 h-5 text-brand-500" />
            Policy Gates
          </h2>
          <p className="text-sm text-text-secondary mt-1">
            Define quality gates that fail when findings exceed a severity threshold.
          </p>
        </div>
        <button
          onClick={handleCreate}
          disabled={creating || !activeOrgId}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium rounded-md bg-brand-500 text-white hover:bg-brand-600 disabled:opacity-40 transition-colors"
        >
          <Plus className="w-4 h-4" />
          New Policy
        </button>
      </div>

      {!activeOrgId && (
        <div className="text-sm text-text-secondary italic">
          Select an organization to manage its policies.
        </div>
      )}

      {activeOrgId && loading && (
        <div className="text-sm text-text-secondary">Loading policies…</div>
      )}

      {activeOrgId && !loading && policies.length === 0 && (
        <div className="text-sm text-text-secondary italic">
          No policies yet. Create one to define a quality gate for your team.
        </div>
      )}

      <div className="space-y-2">
        {policies.map(policy => {
          const isExpanded = expandedId === policy.id
          const enabledCount = policy.rules.filter(r => r.enabled).length
          return (
            <div
              key={policy.id}
              className="border border-border-default rounded-lg overflow-hidden"
            >
              <button
                onClick={() => setExpandedId(isExpanded ? null : policy.id)}
                className="w-full flex items-center justify-between px-4 py-3 hover:bg-surface-2 transition-colors"
              >
                <div className="flex items-center gap-3">
                  {isExpanded ? (
                    <ChevronDown className="w-4 h-4 text-text-secondary" />
                  ) : (
                    <ChevronRight className="w-4 h-4 text-text-secondary" />
                  )}
                  <GateBadge severity={policy.gateSeverity} />
                  <span className="font-medium text-text-primary">{policy.name}</span>
                  <span className="text-xs text-text-secondary">
                    {enabledCount} rule{enabledCount !== 1 ? 's' : ''} enabled
                  </span>
                </div>
                <button
                  onClick={e => {
                    e.stopPropagation()
                    void handleDelete(policy.id)
                  }}
                  className="p-1 text-text-secondary hover:text-red-500 transition-colors"
                  aria-label="Delete policy"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </button>

              {isExpanded && (
                <div className="px-4 py-4 border-t border-border-default space-y-4 bg-surface-1">
                  <div className="grid grid-cols-2 gap-4">
                    <label className="block">
                      <span className="text-xs font-medium text-text-secondary">Name</span>
                      <input
                        type="text"
                        value={policy.name}
                        onChange={e => handleUpdate({...policy, name: e.target.value})}
                        className="mt-1 w-full px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-1 focus:outline-none focus:ring-1 focus:ring-brand-500"
                      />
                    </label>
                    <label className="block">
                      <span className="text-xs font-medium text-text-secondary">Description</span>
                      <input
                        type="text"
                        value={policy.description || ''}
                        onChange={e => handleUpdate({...policy, description: e.target.value})}
                        placeholder="Optional"
                        className="mt-1 w-full px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-1 focus:outline-none focus:ring-1 focus:ring-brand-500"
                      />
                    </label>
                  </div>

                  <div>
                    <span className="text-xs font-medium text-text-secondary">Gate severity (fail threshold)</span>
                    <div className="mt-1">
                      <SegmentedControl
                        options={GATE_OPTIONS}
                        value={policy.gateSeverity ?? ''}
                        onChange={val => handleUpdate({...policy, gateSeverity: val as Severity | undefined})}
                      />
                    </div>
                  </div>

                  <div>
                    <span className="text-xs font-medium text-text-secondary block mb-2">
                      Rules — toggle which rules count toward this gate
                    </span>
                    <div className="max-h-64 overflow-y-auto space-y-1 rounded-md border border-border-default p-2">
                      {ruleDefs.map(rule => {
                        const pr = policy.rules.find(r => r.ruleId === rule.id)
                        const isEnabled = pr?.enabled ?? false
                        return (
                          <div key={rule.id} className="flex items-center gap-3 px-2 py-1.5 rounded hover:bg-surface-2">
                            <button
                              onClick={() => toggleRule(policy, rule.id, !isEnabled)}
                              className={clsx(
                                'w-9 h-5 rounded-full relative transition-colors shrink-0',
                                isEnabled ? 'bg-brand-500' : 'bg-surface-3',
                              )}
                              role="switch"
                              aria-checked={isEnabled}
                              aria-label={`Toggle ${rule.name}`}
                            >
                              <span
                                className={clsx(
                                  'absolute top-0.5 w-4 h-4 rounded-full bg-white transition-transform',
                                  isEnabled ? 'translate-x-4' : 'translate-x-0.5',
                                )}
                              />
                            </button>
                            <span className="text-sm text-text-primary flex-1 truncate">{rule.name}</span>
                            <span className="text-xs text-text-secondary shrink-0">{rule.id}</span>
                            {isEnabled && (
                              <select
                                value={pr?.severity ?? ''}
                                onChange={e => setRuleSeverity(policy, rule.id, e.target.value as Severity)}
                                className="text-xs px-1.5 py-0.5 rounded border border-border-default bg-surface-1 text-text-secondary shrink-0"
                              >
                                <option value="">Default</option>
                                <option value="error">Error</option>
                                <option value="warning">Warning</option>
                                <option value="info">Info</option>
                              </select>
                            )}
                          </div>
                        )
                      })}
                    </div>
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}

function GateBadge({severity}: {severity?: Severity}) {
  if (!severity) {
    return (
      <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-surface-3 text-text-secondary">
        <Shield className="w-3 h-3" /> Report only
      </span>
    )
  }
  if (severity === 'error') {
    return (
      <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-red-500/10 text-red-500">
        <ShieldAlert className="w-3 h-3" /> Error+
      </span>
    )
  }
  if (severity === 'warning') {
    return (
      <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-amber-500/10 text-amber-500">
        <ShieldAlert className="w-3 h-3" /> Warning+
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-blue-500/10 text-blue-500">
      <ShieldCheck className="w-3 h-3" /> Info+
    </span>
  )
}
