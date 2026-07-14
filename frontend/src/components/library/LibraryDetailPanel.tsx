import {Activity, Calendar, FileCode, GitBranch, Trash2, User, Users, X} from 'lucide-react'
import {libraryApi, type LibraryFlow, type LibraryFlowVersion} from '@/api/library'
import {Button, Spinner, ErrorState} from '@/components/shared'
import {logger} from '@/lib/logger'
import {relativeTime, absoluteTime} from '@/lib/time'
import {useOrgStore} from '@/stores/orgStore'
import {useAsync} from '@/hooks/useAsync'

interface Props {
  flowId: string | null
  onOpen: (flow: LibraryFlow) => void
  onDelete: (flow: LibraryFlow) => void
  onClose: () => void
}

interface DetailData {
  flow: LibraryFlow
  versions: LibraryFlowVersion[]
}

export default function LibraryDetailPanel({flowId, onOpen, onDelete, onClose}: Props) {
  const orgs = useOrgStore(s => s.organisations)

  const {
    data,
    isLoading: loading,
    error,
    refetch: load,
  } = useAsync<DetailData | null>(() => {
    if (!flowId) return Promise.resolve(null)
    return Promise.all([libraryApi.get(flowId), libraryApi.versions(flowId, 10).catch(() => [])])
      .then(([flow, versions]) => ({flow, versions}))
      .catch(e => {
        logger.warn('Library: detail load failed', e)
        throw e
      })
  }, [flowId])
  const flow = data?.flow ?? null
  const versions = data?.versions ?? []

  if (!flowId) {
    return <div className="p-6 text-center text-sm text-text-tertiary">Select a flow to see its details.</div>
  }

  if (loading && !flow) {
    return (
      <div className="flex justify-center p-8">
        <Spinner />
      </div>
    )
  }

  if (error) {
    return <ErrorState message={error} onRetry={load} />
  }

  if (!flow) return null

  const orgName = flow.organizationId
    ? (orgs.find(o => o.id === flow.organizationId)?.name ?? 'Unknown org')
    : 'Personal'

  return (
    <div className="flex flex-col h-full">
      <header className="flex items-start justify-between gap-2 px-4 pt-4 pb-2 border-b border-border-subtle flex-shrink-0">
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-text-primary truncate">{flow.name}</h2>
          {flow.description && <p className="mt-1 text-xs text-text-muted line-clamp-3">{flow.description}</p>}
        </div>
        <button
          type="button"
          onClick={onClose}
          className="p-1 text-text-tertiary hover:text-text-primary rounded transition-colors flex-shrink-0"
          aria-label="Close detail panel"
        >
          <X size={14} />
        </button>
      </header>

      <div className="flex-1 overflow-y-auto p-4 space-y-4">
        <Section title="Overview">
          <Row icon={User} label="Owner" value={flow.ownerDisplayName ?? flow.ownerId} />
          <Row icon={Users} label="Organisation" value={orgName} />
          <Row
            icon={Calendar}
            label="Last updated"
            value={<span title={absoluteTime(flow.updatedAt)}>{relativeTime(flow.updatedAt)}</span>}
          />
          <Row icon={FileCode} label="Blocks / subflows" value={`${flow.blockCount} / ${flow.subflowCount}`} />
          <Row icon={GitBranch} label="Version" value={`#${flow.version}`} />
        </Section>

        {flow.healthScore !== undefined && (
          <Section title="Latest analysis">
            <Row
              icon={Activity}
              label="Health score"
              value={
                <span
                  className={
                    flow.healthScore >= 80
                      ? 'text-semantic-success'
                      : flow.healthScore >= 60
                        ? 'text-amber-500'
                        : 'text-semantic-error'
                  }
                >
                  {flow.healthScore}%
                </span>
              }
            />
            <Row icon={Activity} label="Errors" value={String(flow.errorCount ?? 0)} />
            <Row icon={Activity} label="Warnings" value={String(flow.warningCount ?? 0)} />
          </Section>
        )}

        <Section title={`Version history${versions.length ? ` (${versions.length})` : ''}`}>
          {versions.length === 0 ? (
            <p className="text-xs text-text-tertiary">No saved versions yet.</p>
          ) : (
            <ul className="space-y-1.5">
              {versions.map(v => (
                <li key={v.id} className="text-xs text-text-secondary">
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="font-medium text-text-primary">v{v.version}</span>
                    <span className="text-text-tertiary" title={absoluteTime(v.createdAt)}>
                      {relativeTime(v.createdAt)}
                    </span>
                  </div>
                  {v.comment && <div className="text-text-muted truncate">{v.comment}</div>}
                </li>
              ))}
            </ul>
          )}
        </Section>

        <div className="flex items-center gap-2 pt-3 border-t border-border-subtle">
          {flow.canDelete && (
            <Button variant="ghost" size="sm" icon={Trash2} onClick={() => onDelete(flow)}>
              Delete
            </Button>
          )}
          <Button variant="primary" size="sm" onClick={() => onOpen(flow)} fullWidth>
            Load flow
          </Button>
        </div>
      </div>
    </div>
  )
}

function Section({title, children}: {title: string; children: React.ReactNode}) {
  return (
    <div>
      <div className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">{title}</div>
      <div className="space-y-1.5">{children}</div>
    </div>
  )
}

function Row({icon: Icon, label, value}: {icon: typeof Activity; label: string; value: React.ReactNode}) {
  return (
    <div className="flex items-baseline gap-2 text-xs">
      <Icon size={11} className="text-text-tertiary translate-y-0.5 flex-shrink-0" />
      <span className="text-text-tertiary w-28 flex-shrink-0">{label}</span>
      <span className="text-text-primary truncate">{value}</span>
    </div>
  )
}
