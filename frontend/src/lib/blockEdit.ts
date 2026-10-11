import {analysisApi} from '@/api'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {registerStoreReset} from '@/stores/storeRegistry'
import {logger} from '@/lib/logger'
import type {Block, FlowDocument} from '@/types'

export const REANALYZE_DEBOUNCE_MS = 400

export const MAX_QUEUED_EDITS = 8

export function refreshAfterBlockEdit(updated: FlowDocument) {
  const st = useFlowStore.getState()
  if (st.document?.id !== updated.id) return
  st.applyRemoteDocumentUpdate(updated)

  const {selectedBlockIds, renamingBlockId} = st
  if (selectedBlockIds.size > 0 || renamingBlockId) {
    const present = new Set<string>()
    for (const sf of updated.subflows) collectBlockIds(sf.blocks, present)
    const kept = [...selectedBlockIds].filter(id => present.has(id))
    if (kept.length !== selectedBlockIds.size) st.setBlockSelection(new Set(kept))
    if (renamingBlockId && !present.has(renamingBlockId)) st.setRenamingBlock(null)
  }

  scheduleReanalysis(updated.id)
}

const reanalysisTimers = new Map<string, ReturnType<typeof setTimeout>>()

export function clearReanalysisTimers(): void {
  for (const t of reanalysisTimers.values()) clearTimeout(t)
  reanalysisTimers.clear()
}
registerStoreReset(clearReanalysisTimers)

function scheduleReanalysis(flowId: string) {
  const pending = reanalysisTimers.get(flowId)
  if (pending) clearTimeout(pending)
  reanalysisTimers.set(
    flowId,
    setTimeout(() => {
      reanalysisTimers.delete(flowId)
      analysisApi
        .analyzeFlowById(flowId)
        .then(r => {
          if (r) useAnalysisStore.getState().setReport(flowId, r)
        })
        .catch(() => {})
    }, REANALYZE_DEBOUNCE_MS),
  )
}

function collectBlockIds(blocks: Block[], out: Set<string>) {
  for (const b of blocks) {
    out.add(b.id)
    if (b.children?.length) collectBlockIds(b.children, out)
  }
}

interface EditLane {
  busy: boolean
  queue: Array<() => void>
  issued: number
  applied: number
  newest: {seq: number; document: FlowDocument} | null
}

const lanes = new Map<string, EditLane>()

function applyNewest(lane: EditLane): void {
  const newest = lane.newest
  lane.newest = null
  if (!newest || newest.seq <= lane.applied) return
  lane.applied = newest.seq
  try {
    refreshAfterBlockEdit(newest.document)
  } catch (err) {
    logger.error('Failed to apply an edited document', err)
  }
}

export function runBlockEdit<T extends {document?: FlowDocument} | undefined>(
  flowId: string,
  edit: () => Promise<T>,
): Promise<T | undefined> {
  let lane = lanes.get(flowId)
  if (!lane) {
    lane = {busy: false, queue: [], issued: 0, applied: 0, newest: null}
    lanes.set(flowId, lane)
  }
  if (lane.queue.length >= MAX_QUEUED_EDITS) return Promise.resolve(undefined)
  const current = lane
  const seq = ++current.issued

  return new Promise<T | undefined>((resolve, reject) => {
    const start = () => {
      current.busy = true
      let pending: Promise<T>
      try {
        pending = edit()
      } catch (err) {
        pending = Promise.reject(err)
      }
      void pending
        .then(
          value => {
            if (value?.document && seq > (current.newest?.seq ?? current.applied)) {
              current.newest = {seq, document: value.document}
            }
            return {ok: true as const, value}
          },
          (error: unknown) => ({ok: false as const, error}),
        )
        .then(outcome => {
          if (current.queue.length === 0) applyNewest(current)
          const next = current.queue.shift()
          if (next) {
            next()
          } else {
            current.busy = false
            if (lanes.get(flowId) === current) lanes.delete(flowId)
          }
          if (outcome.ok) resolve(outcome.value)
          else reject(outcome.error)
        })
    }
    if (current.busy) current.queue.push(start)
    else start()
  })
}
