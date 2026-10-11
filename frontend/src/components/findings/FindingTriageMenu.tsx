import {useState, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
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
import {useSuppressReasonPrompt} from './hooks/useSuppressReasonPrompt'
import {TRIAGE_STATUSES, isKnown, type Finding, type Tolerant, type TriageStatus} from '@/types'

const TRIAGE_OPTIONS: TriageStatus[] = ['open', 'acknowledged', 'in_progress', 'resolved', 'suppressed']

interface AssignableUser {
  id: string
  displayName: string
  avatarUrl?: string
}

interface Props {
  finding: Finding
}

export default function FindingTriageMenu({finding}: Props) {
  const {t} = useTranslation('findings')
  const [showTriage, setShowTriage] = useState(false)
  const [showAssign, setShowAssign] = useState(false)
  const toast = useToast()
  const promptSuppressReason = useSuppressReasonPrompt()
  const setFindingTriage = useAnalysisStore(s => s.setFindingTriage)
  const assignFinding = useAnalysisStore(s => s.assignFinding)
  const triage = useAnalysisStore(s => s.triageMap.get(findingKey(finding)))
  const triageStatus: Tolerant<TriageStatus> = triage?.status ?? 'open'
  const knownStatus = isKnown(TRIAGE_STATUSES, triageStatus)
  const organisations = useOrgStore(s => s.organisations)
  const currentUser = useAuthStore(s => s.user)
  const isDesktop = isTauri()

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

  const triageRef = useDismissable(showTriage || showAssign, () => {
    setShowTriage(false)
    setShowAssign(false)
  })

  const handleSetTriage = async (status: TriageStatus) => {
    setShowTriage(false)
    if (status === triageStatus) return
    let justification: string | undefined
    if (status === 'suppressed') {
      const reason = await promptSuppressReason()
      if (reason === null) return
      justification = reason
    }
    setFindingTriage(finding, status, justification)
    if (status === 'suppressed') {
      toast.info(t('toasts.findingSuppressed'), {
        action: {label: t('toasts.undo'), onClick: () => setFindingTriage(finding, 'open')},
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
          title={
            [
              triage?.justification ? t('triage.labels.justification', {reason: triage.justification}) : '',
              isDesktop ? t('triage.labels.sessionOnlyHint') : '',
            ]
              .filter(Boolean)
              .join('\n') || undefined
          }
        >
          {knownStatus ? t(`triage.status.${triageStatus}`) : t('triage.status.other')}
          {isDesktop && (
            <span className="ml-1 normal-case tracking-normal opacity-70">· {t('triage.labels.sessionOnly')}</span>
          )}
        </span>
      )}

      {assignee && (
        <span
          className="flex items-center gap-1 text-2xs text-text-tertiary"
          title={t('triage.labels.assignedTo', {name: assignee.displayName})}
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
          title={
            isDesktop
              ? `${t('triage.actions.setStatus')} (${t('triage.labels.sessionOnly')})`
              : t('triage.actions.setStatus')
          }
        >
          <CircleDot size={10} />
        </button>
        {showTriage && (
          <div className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-36">
            {isDesktop && (
              <div
                className="px-2.5 py-1 text-2xs text-text-tertiary border-b border-border-subtle mb-0.5"
                title={t('triage.labels.sessionOnlyHint')}
              >
                {t('triage.labels.sessionOnly')}
              </div>
            )}
            {TRIAGE_OPTIONS.map(s => (
              <button
                key={s}
                onClick={() => void handleSetTriage(s)}
                className={clsx(
                  'block w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors',
                  triageStatus === s ? 'text-brand-400 font-medium' : 'text-text-secondary',
                )}
              >
                {t(`triage.status.${s}`)}
              </button>
            ))}

            {!isDesktop && knownStatus && assignees.length > 0 && (
              <>
                <div className="my-0.5 border-t border-border-subtle" />
                <div className="relative" onMouseEnter={() => setShowAssign(true)}>
                  <button
                    onClick={() => setShowAssign(s => !s)}
                    className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors text-text-secondary"
                  >
                    <UserPlus size={10} />
                    {t('triage.actions.assignTo')}
                  </button>
                  {showAssign && (
                    <div className="absolute left-full top-0 ml-0.5 z-30 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-40 max-h-56 overflow-auto">
                      {triage?.assigneeId && (
                        <button
                          onClick={() => handleAssign(null)}
                          className="flex items-center gap-1.5 w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors text-text-tertiary"
                        >
                          <UserMinus size={10} />
                          {t('triage.actions.unassign')}
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
                          <span className="truncate">{t('triage.actions.assignToMe')}</span>
                        </button>
                      )}
                      <div className="not-first:border-t border-border-subtle" />
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
