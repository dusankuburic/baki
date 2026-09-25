import {useTranslation} from 'react-i18next'
import {AlertTriangle, Key, Lock, ShieldAlert} from 'lucide-react'
import {StatTile} from '@/components/charts'
import type {DashboardAnalyticsData} from '@/types'
import {ChartSection} from '../ChartSection'

// Sign-in and secret-handling signals as tiles; the locked-account count is
// instance-wide and only arrives for admins.
export function SecuritySection({data}: {data: DashboardAnalyticsData}) {
  const {t} = useTranslation('dashboard')
  const sec = data.security
  const unavailable = sec.available ? undefined : t('securityTiles.unavailable')
  return (
    <ChartSection
      id="analytics-security"
      title={t('analytics.sections.security')}
      description={t('analytics.sectionSub.security')}
    >
      <div className="analytics-tiles analytics-span-12">
        <StatTile
          label={t('securityTiles.failedLogins')}
          value={String(sec.failedLogins24h)}
          sub={t('securityTiles.yourAccount')}
          icon={AlertTriangle}
          tone={
            sec.failedLogins24h > 5
              ? 'text-semantic-error'
              : sec.failedLogins24h > 0
                ? 'text-semantic-warning'
                : undefined
          }
          unavailable={unavailable}
        />
        <StatTile
          label={t('securityTiles.securityFindings')}
          value={String(sec.securityFindings)}
          sub={t('securityTiles.inScope')}
          icon={ShieldAlert}
          tone={sec.securityFindings > 0 ? 'text-semantic-warning' : undefined}
          unavailable={unavailable}
        />
        <StatTile
          label={t('securityTiles.credentialFindings')}
          value={String(sec.credentialFindings)}
          sub={t('securityTiles.inScope')}
          icon={Key}
          tone={sec.credentialFindings > 0 ? 'text-semantic-error' : undefined}
          unavailable={unavailable}
        />
        {sec.lockedAccounts !== null && (
          <StatTile
            label={t('securityTiles.lockedAccounts')}
            value={String(sec.lockedAccounts)}
            sub={t('securityTiles.lockedAdminOnly')}
            icon={Lock}
            tone={sec.lockedAccounts > 0 ? 'text-semantic-error' : undefined}
            unavailable={unavailable}
          />
        )}
      </div>
    </ChartSection>
  )
}
