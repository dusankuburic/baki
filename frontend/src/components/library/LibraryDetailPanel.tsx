import {useState} from 'react'
import {Activity, Calendar, Copy, FileCode, GitBranch, Pencil, Tag, Trash2, User, Users, X} from 'lucide-react'
import {libraryApi, type LibraryFlow, type LibraryFlowVersion} from '@/api/library'
import {Button, Spinner, ErrorState} from '@/components/shared'
import {logger} from '@/lib/logger'
import {relativeTime, absoluteTime} from '@/lib/time'
import {healthTier} from '@/lib/scoring'
import {useOrgStore} from '@/stores/orgStore'
import {useAsync} from '@/hooks/useAsync'
import {isTauri} from '@/platform/guards'

interface Props {
  flowId: string | null
  // Changing this re-reads the flow (after a rename, tag edit or conflict
  // reload elsewhere in the workspace).
  reloadKey?: number
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
  // Set when the versions call failed. Rendered as an error, never as an
  // empty list: "No saved versions yet." over a 503 told the user their
  // history was empty when the feature was unavailable.
  versionsError: string | null
}

// Local mode (desktop) has no version store — the server answers 503 — so
// the section is not rendered at all rather than shown empty or erroring.
const versioningAvailable = () => !isTauri()

export default function LibraryDetailPanel({
  flowId,
  reloadKey = 0,
  onOpen,
  onDelete,
  onRename,
  onDuplicate,
  onSaveTags,
  editingTags,
  setEditingTags,
  onClose,
}: Props) {
  const orgs = useOrgStore(s => s.organisations)

  const {
    data,
    isLoading: loading,
    error,
    refetch: load,
  } = useAsync<DetailData | null>(() => {
    if (!flowId) return Promise.resolve(null)
    const versionsP: Promise<Pick<DetailData, 'versions' | 'versionsError'>> = versioningAvailable()
      ? libraryApi.versions(flowId, 10).then(
          versions => ({versions, versionsError: null}),
          (e: unknown) => {
            logger.warn('Library: versions load failed', e)
            return {versions: [], versionsError: e instanceof Error ? e.message : String(e)}
          },
        )
      : Promise.resolve({versions: [], versionsError: null})
    return Promise.all([libraryApi.get(flowId), versionsP])
      .then(([flow, v]) => ({flow, ...v}))
      .catch(e => {
        logger.warn('Library: detail load failed', e)
        throw e
      })
  }, [flowId, reloadKey])
  const flow = data?.flow ?? null
  const versions = data?.versions ?? []
  const versionsError = data?.versionsError ?? null

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
              value={<span className={healthTier(flow.healthScore).text}>{flow.healthScore}%</span>}
            />
            <Row icon={Activity} label="Errors" value={String(flow.errorCount ?? 0)} />
            <Row icon={Activity} label="Warnings" value={String(flow.warningCount ?? 0)} />
          </Section>
        )}

        {versioningAvailable() && (
          <Section title={`Version history${versions.length ? ` (${versions.length})` : ''}`}>
            {versionsError ? (
              <p className="text-xs text-semantic-error" role="alert">
                Version history unavailable: {versionsError}
              </p>
            ) : versions.length === 0 ? (
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
        )}

        <div className="flex items-center gap-2 pt-3 border-t border-border-subtle flex-wrap">
          {/* Viewers get neither: the server refuses the rename, and a copy
              lands in the same org, where a viewer cannot create flows. */}
          {flow.canEdit && (
            <>
              <Button variant="ghost" size="sm" icon={Pencil} onClick={() => onRename(flow)}>
                Rename
              </Button>
              <Button variant="ghost" size="sm" icon={Copy} onClick={() => onDuplicate(flow)}>
                Duplicate
              </Button>
            </>
          )}
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
function TagRow({
  flow,
  editing,
  setEditing,
  onSave,
}: {
  flow: LibraryFlow
  editing: boolean
  setEditing: (v: boolean) => void
  onSave: (flow: LibraryFlow, tags: string[]) => void
}) {
  const [draft, setDraft] = useState('')
  // Seed the draft when edit mode opens (or the underlying tags change), adjusted
  // during render. The composite key reproduces the previous effect's [editing,
  // flow.tags] trigger exactly.
  const tagsText = (flow.tags ?? []).join(', ')
  const seedKey = `${editing}|${tagsText}`
  const [lastSeedKey, setLastSeedKey] = useState<string | null>(null)
  if (seedKey !== lastSeedKey) {
    setLastSeedKey(seedKey)
    if (editing) setDraft(tagsText)
  }

  if (editing) {
    return (
      <div className="pt-1">
        <input
          value={draft}
          onChange={(e: React.ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') {
              onSave(
                flow,
                draft
                  .split(',')
                  .map((t: string) => t.trim())
                  .filter(Boolean),
              )
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
              onSave(
                flow,
                draft
                  .split(',')
                  .map((t: string) => t.trim())
                  .filter(Boolean),
              )
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
    // `group` is what the edit button's hover reveal keys off; without it the
    // button stayed at opacity 0 for mouse users.
    <div className="group flex items-start gap-2 pt-1 flex-wrap">
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
          className="p-0.5 rounded text-text-tertiary hover:text-text-secondary opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity"
          title="Edit tags"
          aria-label="Edit tags"
        >
          <Tag size={10} />
        </button>
      )}
    </div>
  )
}
