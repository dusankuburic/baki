import {ShieldAlert, AlertTriangle, Key} from 'lucide-react'
import {CardShell} from './CardShell'
import type {DashboardSecurity} from '@/types'

export function SecurityPostureCard({data, className}: {data: DashboardSecurity; className?: string}) {
  const hasIssues = data.failedLogins24h > 0 || data.credentialFindings > 0

  const items = [
    {
      icon: AlertTriangle,
      label: 'Failed logins (24h)',
      value: data.failedLogins24h,
      tone: data.failedLogins24h > 5 ? 'text-red-400' : data.failedLogins24h > 0 ? 'text-amber-400' : 'text-emerald-400',
    },
    {
      icon: Key,
      label: 'Credential findings',
      value: data.credentialFindings,
      tone: data.credentialFindings > 0 ? 'text-red-400' : 'text-emerald-400',
    },
  ]

  return (
    <CardShell title="Security Posture" className={className}>
      {!hasIssues ? (
        <div className="flex flex-col items-center justify-center h-32 gap-2">
          <ShieldAlert size={20} className="text-emerald-400" />
          <span className="text-xs text-text-tertiary text-center">No security issues detected</span>
        </div>
      ) : (
        <div className="flex flex-col gap-3 py-2">
          {items.map((item) => (
            <div key={item.label} className="flex items-center gap-3">
              <div className="w-9 h-9 rounded-lg bg-surface-3 flex items-center justify-center shrink-0">
                <item.icon size={15} className={item.tone} />
              </div>
              <div className="flex-1">
                <div className="text-sm font-mono font-bold tabular-nums" style={{color: 'var(--text-primary)'}}>
                  <span className={item.tone}>{item.value}</span>
                </div>
                <div className="text-2xs text-text-tertiary">{item.label}</div>
              </div>
            </div>
          ))}
        </div>
      )}
    </CardShell>
  )
}
