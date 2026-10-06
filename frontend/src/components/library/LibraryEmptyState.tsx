import {FolderOpen, SearchX, FilterX} from 'lucide-react'
import {useTranslation} from 'react-i18next'
import type {LibraryScope} from '@/api/library'

interface Props {
  scope: LibraryScope
  hasQuery: boolean
  hasOrgFilter: boolean
}

export default function LibraryEmptyState({scope, hasQuery, hasOrgFilter}: Props) {
  const {t} = useTranslation('library')
  if (hasQuery) {
    return (
      <Centered
        icon={SearchX}
        title={t('empty.noMatchTitle')}
        subtitle={t('empty.noMatchSubtitle')}
      />
    )
  }
  if (hasOrgFilter) {
    return (
      <Centered
        icon={FilterX}
        title={t('empty.scopeTitle')}
        subtitle={t('empty.scopeSubtitle')}
      />
    )
  }
  if (scope === 'shared') {
    return (
      <Centered
        icon={FolderOpen}
        title={t('empty.sharedTitle')}
        subtitle={t('empty.sharedSubtitle')}
      />
    )
  }
  if (scope === 'mine') {
    return <Centered icon={FolderOpen} title={t('empty.mineTitle')} subtitle={t('empty.saveHint')} />
  }
  return <Centered icon={FolderOpen} title={t('empty.noneTitle')} subtitle={t('empty.saveHint')} />
}

function Centered({icon: Icon, title, subtitle}: {icon: typeof FolderOpen; title: string; subtitle: string}) {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-center">
      <Icon size={28} className="text-text-tertiary mb-3 opacity-60" />
      <p className="text-sm font-medium text-text-secondary">{title}</p>
      <p className="mt-1 text-xs text-text-tertiary max-w-xs">{subtitle}</p>
    </div>
  )
}
