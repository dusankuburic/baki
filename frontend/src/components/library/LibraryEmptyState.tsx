import {FolderOpen, SearchX, FilterX} from 'lucide-react'
import type {LibraryScope} from '@/api/library'

interface Props {
  scope: LibraryScope
  hasQuery: boolean
  hasOrgFilter: boolean
}

export default function LibraryEmptyState({scope, hasQuery, hasOrgFilter}: Props) {
  if (hasQuery) {
    return <Centered icon={SearchX} title="No matching flows" subtitle="Try a different search term." />
  }
  if (hasOrgFilter) {
    return (
      <Centered icon={FilterX} title="No flows in this scope" subtitle="Adjust the filter rail to widen the results." />
    )
  }
  if (scope === 'shared') {
    return (
      <Centered
        icon={FolderOpen}
        title="Nothing shared with you yet"
        subtitle="Flows shared by collaborators will appear here."
      />
    )
  }
  if (scope === 'mine') {
    return (
      <Centered
        icon={FolderOpen}
        title="You haven't saved any flows yet"
        subtitle="Save a flow to the cloud library from the sidebar."
      />
    )
  }
  return (
    <Centered icon={FolderOpen} title="No flows yet" subtitle="Save a flow to the cloud library from the sidebar." />
  )
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
