import {useTranslation} from 'react-i18next'
import {ShieldCheck, AlertTriangle, Key, Lock} from 'lucide-react'
import {ChartCard, StatTile} from '@/components/charts'
import {DetailsAction} from './DetailsAction'
import type {DashboardSecurity} from '@/types'

// Sign-in and secret-handling signals as tiles. Locked accounts is an
// instance-wide count and only shows for admins (the payload says so).
export function SecurityPostureCard({
  data,
  onDetails,
  className,
}: {
  data: DashboardSecurity
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const title = t('home.security')
  const hasIssues = data.failedLogins24h > 0 || data.credentialFindings > 0 || (data.admin && data.lockedAccounts > 0)
  return (
    <ChartCard
      title={title}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={data.available ? 'ready' : 'empty'}
      emptyMessage={t('securityTiles.unavailable')}
      className={className}
    >
      {!hasIssues && (
        <p className="mb-3 flex items-center gap-2 text-xs text-semantic-success">
          <ShieldCheck size={14} aria-hidden="true" />
          {t('securityTiles.allClear')}
        </p>
      )}
      <div className="grid grid-cols-2 gap-2">
        <StatTile
          label={t('securityTiles.failedLogins')}
          value={String(data.failedLogins24h)}
          sub={t('securityTiles.yourAccount')}
          icon={AlertTriangle}
          tone={
            data.failedLogins24h > 5
              ? 'text-semantic-error'
              : data.failedLogins24h > 0
                ? 'text-semantic-warning'
                : undefined
          }
        />
        <StatTile
          label={t('securityTiles.credentialFindings')}
          value={String(data.credentialFindings)}
          sub={t('securityTiles.inScope')}
          icon={Key}
          tone={data.credentialFindings > 0 ? 'text-semantic-error' : undefined}
        />
        {data.admin && (
          <StatTile
            label={t('securityTiles.lockedAccounts')}
            value={String(data.lockedAccounts)}
            sub={t('securityTiles.lockedAdminOnly')}
            icon={Lock}
            tone={data.lockedAccounts > 0 ? 'text-semantic-error' : undefined}
          />
        )}
      </div>
    </ChartCard>
  )
}
