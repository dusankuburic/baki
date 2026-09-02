import {useState, useEffect} from 'react'
import {Activity, Calendar, Copy, FileCode, GitBranch, Pencil, Tag, Trash2, User, Users, X} from 'lucide-react'
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
  onRename: (flow: LibraryFlow) => void
  onDuplicate: (flow: LibraryFlow) => void
  onSaveTags: (flow: LibraryFlow, tags: string[]) => void
  editingTags: boolean
  setEditingTags: (v: boolean) => void
  onClose: () => void
}

interface DetailData {
  flow: LibraryFlow
  versions: LibraryFlowVersion[]
}

export default function LibraryDetailPanel({flowId, onOpen, onDelete, onRename, onDuplicate, onSaveTags, editingTags, setEditingTags, onClose}: Props) {
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
          {flow.description && <p className="mt-1 text-xs text-text-tertiary line-clamp-3">{flow.description}</p>}
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
          <TagRow flow={flow} editing={editingTags} setEditing={setEditingTags} onSave={onSaveTags} />
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
                  {v.comment && <div className="text-text-tertiary truncate">{v.comment}</div>}
                </li>
              ))}
            </ul>
          )}
        </Section>

        <div className="flex items-center gap-2 pt-3 border-t border-border-subtle flex-wrap">
          <Button variant="ghost" size="sm" icon={Pencil} onClick={() => onRename(flow)}>
            Rename
          </Button>
          <Button variant="ghost" size="sm" icon={Copy} onClick={() => onDuplicate(flow)}>
            Duplicate
          </Button>
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


// TagRow renders the flow's tags as chips with an inline editor
// (comma-separated input → server-normalized set on save). Editing is
// triggered by the pencil; empty input saves an empty set (untagged).
function TagRow({flow, editing, setEditing, onSave}: {flow: LibraryFlow; editing: boolean; setEditing: (v: boolean) => void; onSave: (flow: LibraryFlow, tags: string[]) => void}) {
  const [draft, setDraft] = useState('')
  useEffect(() => {
    if (editing) setDraft((flow.tags ?? []).join(', '))
  }, [editing, flow.tags])

  if (editing) {
    return (
      <div className="pt-1">
        <input
          value={draft}
          onChange={(e: React.ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') {
              onSave(flow, draft.split(',').map((t: string) => t.trim()).filter(Boolean))
              setEditing(false)
            }
            if (e.key === 'Escape') setEditing(false)
          }}
          placeholder="prod, finance, critical — Enter to save"
          autoFocus
          className="w-full px-2 py-1 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500"
          aria-label="Tags (comma-separated)"
        />
        <div className="flex gap-2 mt-1">
          <button
            className="text-2xs text-brand-400 hover:text-brand-300"
            onClick={() => {
              onSave(flow, draft.split(',').map((t: string) => t.trim()).filter(Boolean))
              setEditing(false)
            }}
          >
            Save
          </button>
          <button className="text-2xs text-text-tertiary hover:text-text-secondary" onClick={() => setEditing(false)}>
            Cancel
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex items-start gap-2 pt-1 flex-wrap">
      {(flow.tags ?? []).length === 0 ? (
        <span className="text-xs text-text-tertiary/60">No tags</span>
      ) : (
        (flow.tags ?? []).map(t => (
          <span key={t} className="px-1.5 py-0.5 rounded bg-brand-500/10 text-brand-300 text-2xs font-medium">
            {t}
          </span>
        ))
      )}
      {flow.canEdit && (
        <button
          onClick={() => setEditing(true)}
          className="p-0.5 rounded text-text-tertiary hover:text-text-secondary opacity-0 group-hover:opacity-100 transition-opacity"
          title="Edit tags"
          aria-label="Edit tags"
        >
          <Tag size={10} />
        </button>
      )}
    </div>
  )
}
