import {useState, useMemo} from 'react'
import clsx from 'clsx'
import {CircleDot, UserPlus, UserMinus, X} from 'lucide-react'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useToast} from '@/components/shared'
import {useDismissable} from '@/hooks/useDismissable'
import {triageTone} from '@/lib/severityTone'
import Avatar from '@/components/shared/Avatar'
import {isTauri} from '@/platform/guards'
import type {Finding, TriageStatus} from '@/types'

const TRIAGE_OPTIONS: {s: TriageStatus; label: string}[] = [
  {s: 'open', label: 'Open'},
  {s: 'acknowledged', label: 'Acknowledged'},
  {s: 'in_progress', label: 'In Progress'},
  {s: 'resolved', label: 'Resolved'},
  {s: 'suppressed', label: 'Suppressed'},
]

interface AssignableUser {
  id: string
  displayName: string
  avatarUrl?: string
}

interface Props {
  finding: Finding
}

export default function FindingTriageMenu({finding}: Props) {
  const [showTriage, setShowTriage] = useState(false)
  const [showAssign, setShowAssign] = useState(false)
  const toast = useToast()
  const setFindingTriage = useAnalysisStore(s => s.setFindingTriage)
  const assignFinding = useAnalysisStore(s => s.assignFinding)
  // Select only the single triage entry this card needs — selecting the whole
  // triageMap would re-render every FindingCard on any triage change (O(N)).
  const triage = useAnalysisStore(s => s.triageMap.get(findingKey(finding)))
  const triageStatus: TriageStatus = triage?.status ?? 'open'
  const organisations = useOrgStore(s => s.organisations)
  const currentUser = useAuthStore(s => s.user)
  const isDesktop = isTauri()

  // Flatten all org members into a deduped id→user map for the assignee picker.
  // Assignment is a cloud-only feature (desktop triage is in-memory and has no
  // notion of other users), so the picker is hidden in Tauri mode below.
  const assignees = useMemo<AssignableUser[]>(() => {
    const map = new Map<string, AssignableUser>()
    for (const org of organisations) {
      for (const m of org.members ?? []) {
        if (!map.has(m.userId)) {
          map.set(m.userId, {
            id: m.userId,
            displayName: m.user?.displayName || m.user?.email || m.userId,
            avatarUrl: m.user?.avatarUrl,
          })
        }
      }
    }
    return [...map.values()].sort((a, b) => a.displayName.localeCompare(b.displayName))
  }, [organisations])

  const assignee = triage?.assigneeId ? assignees.find(a => a.id === triage.assigneeId) : undefined

  // Shared dismissal contract (U1.5): outside click + Escape close both the
  // triage menu and its assignee submenu.
  const triageRef = useDismissable(showTriage || showAssign, () => {
    setShowTriage(false)
    setShowAssign(false)
  })

  const handleSetTriage = (status: TriageStatus) => {
    setShowTriage(false)
    if (status === triageStatus) return
    setFindingTriage(finding, status)
    if (status === 'suppressed') {
      toast.info('Finding suppressed', {
        action: {label: 'Undo', onClick: () => setFindingTriage(finding, 'open')},
      })
    }
  }

  const handleAssign = (userId: string | null) => {
    setShowTriage(false)
    setShowAssign(false)
    assignFinding(finding, userId)
  }

  return (
    <>
      {triageStatus !== 'open' && (
        <span
          className={clsx(
            'text-2xs uppercase tracking-wider px-1.5 py-0.5 rounded border',
            triageTone(triageStatus).text,
            triageTone(triageStatus).border,
            triageTone(triageStatus).bg,
          )}
        >
          {triageStatus.replace('_', ' ')}
        </span>
      )}

      {/* Assignee chip — shown only when someone is assigned. */}
      {assignee && (
        <span
          className="flex items-center gap-1 text-2xs text-text-tertiary"
          title={`Assigned to ${assignee.displayName}`}
        >
          <Avatar
            name={assignee.displayName}
            colorSeed={assignee.id}
            avatarUrl={assignee.avatarUrl}
            size="sm"
            className="w-4 h-4 text-2xs"
          />
          <span className="max-w-16 truncate">{assignee.displayName}</span>
        </span>
      )}

      <div className="relative" ref={triageRef}>
        <button
          onClick={() => {
            setShowTriage(s => !s)
            setShowAssign(false)
          }}
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
          title="Set triage status"
        >
          <CircleDot size={10} />
        </button>
        {showTriage && (
          <div className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-36">
            {TRIAGE_OPTIONS.map(({s, label}) => (
              <button
                key={s}
                onClick={() => handleSetTriage(s)}
                className={clsx(
                  'block w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors',
                  triageStatus === s ? 'text-brand-400 font-medium' : 'text-text-secondary',
                )}
              >
                {label}
              </button>
            ))}

            {/* Assignment lives behind a divider + submenu. Cloud-only: desktop
                mode has no notion of other users, so the picker is hidden. */}
            {!isDesktop && assignees.length > 0 && (
              <>
                <div className="my-0.5 border-t border-border-subtle" />
                {/* Hover PEEKS into the submenu; only click/outside/Esc close
                    it — a mouse-leave slam-shut made keyboard assignment
                    unreachable (the submenu vanished before its buttons
                    could take focus). */}
                <div className="relative" onMouseEnter={() => setShowAssign(true)}>
                  <button
                    onClick={() => setShowAssign(s => !s)}
                    className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors text-text-secondary"
                  >
                    <UserPlus size={10} />
                    Assign to…
                  </button>
                  {showAssign && (
                    <div className="absolute left-full top-0 ml-0.5 z-30 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-40 max-h-56 overflow-auto">
                      {triage?.assigneeId && (
                        <button
                          onClick={() => handleAssign(null)}
                          className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors text-text-tertiary"
                        >
                          <UserMinus size={10} />
                          Unassign
                        </button>
                      )}
                      {currentUser && triage?.assigneeId !== currentUser.id && (
                        <button
                          onClick={() => handleAssign(currentUser.id)}
                          className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors text-text-secondary"
                        >
                          <Avatar
                            name={currentUser.displayName || currentUser.email}
                            colorSeed={currentUser.id}
                            avatarUrl={currentUser.avatarUrl}
                            size="sm"
                            className="w-4 h-4 text-2xs"
                          />
                          <span className="truncate">Me</span>
                        </button>
                      )}
                      <div className="[&:not(:first-child)]:border-t border-border-subtle" />
                      {assignees
                        .filter(a => a.id !== currentUser?.id)
                        .map(a => (
                          <button
                            key={a.id}
                            onClick={() => handleAssign(a.id)}
                            className={clsx(
                              'flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors',
                              triage?.assigneeId === a.id ? 'text-brand-400 font-medium' : 'text-text-secondary',
                            )}
                          >
                            <Avatar
                              name={a.displayName}
                              colorSeed={a.id}
                              avatarUrl={a.avatarUrl}
                              size="sm"
                              className="w-4 h-4 text-2xs"
                            />
                            <span className="truncate">{a.displayName}</span>
                            {triage?.assigneeId === a.id && <X size={9} className="ml-auto opacity-50" />}
                          </button>
                        ))}
                    </div>
                  )}
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </>
  )
}
