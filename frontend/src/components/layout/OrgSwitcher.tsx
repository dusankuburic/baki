import {useTranslation} from 'react-i18next'
import {Building2, ChevronDown} from 'lucide-react'
import {isTauri} from '@/platform/guards'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useUIStore} from '@/stores/uiStore'
import Dropdown, {type DropdownItem} from '@/components/shared/Dropdown'

// OrgSwitcher selects the active organization context for org-scoped views
// (library, knowledge base, org settings). Cloud/web only — the desktop app
// is single-user with no org concept.
//
// Org loading is handled by ProtectedRoute so orgs are available before any
// data-fetching components mount.
export default function OrgSwitcher() {
  const {t} = useTranslation('shell')
  const organisations = useOrgStore(s => s.organisations)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const setActiveOrg = useOrgStore(s => s.setActiveOrg)
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const toggleSettings = useUIStore(s => s.toggleSettings)

  if (isTauri() || !isAuthenticated) return null

  const activeOrg = organisations.find(o => o.id === activeOrgId)

  const items: DropdownItem[] = [
    {
      type: 'item',
      label: activeOrgId === null ? t('org.personalActive') : t('org.personal'),
      onSelect: () => setActiveOrg(null),
    },
    ...organisations.map<DropdownItem>(org => ({
      type: 'item' as const,
      label: org.id === activeOrgId ? `✓ ${org.name}` : org.name,
      onSelect: () => setActiveOrg(org.id),
    })),
    {type: 'separator'},
    {
      type: 'item',
      label: t('org.manage'),
      onSelect: () => toggleSettings(),
    },
  ]

  return (
    <Dropdown
      trigger={
        <button
          className="flex items-center gap-1 h-6 px-2 rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast text-xs"
          title={t('org.switch')}
        >
          <Building2 size={12} />
          <span className="max-w-[120px] truncate">{activeOrg ? activeOrg.name : t('org.personal')}</span>
          <ChevronDown size={10} />
        </button>
      }
      items={items}
      align="end"
    />
  )
}
