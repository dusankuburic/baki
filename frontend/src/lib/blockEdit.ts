import {analysisApi} from '@/api'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import {logger} from '@/lib/logger'
import type {Block, FlowDocument} from '@/types'

/** Trailing debounce for the post-edit re-analysis: one run per burst of edits. */
export const REANALYZE_DEBOUNCE_MS = 400

/**
 * Edits waiting behind the in-flight one, per flow, beyond which new edits
 * are dropped: a held move key repeats ~30×/s against a round trip of tens
 * to hundreds of ms, and an unbounded queue would keep moving the block for
 * seconds after the key came up.
 */
export const MAX_QUEUED_EDITS = 8

// refreshAfterBlockEdit pushes a server-confirmed SAME-FLOW mutation into the
// local stores. Two correctness rules:
//   1. Doc-switch race guard — if the user opened a different flow while the
//      request was in flight, the stale result must NOT replace their doc.
//   2. Same-flow refresh rides applyRemoteDocumentUpdate (NOT setDocument):
//      it preserves the active subflow, chat thread, navigation history, and
//      a still-existing block selection — setDocument reset all of that,
//      teleporting every edit back to subflow #1 and killing the chat.
// Re-analysis targets the UPDATED flow id — analyzeFlow() resolves the
// currently-active doc at call time and could analyze (and store findings
// for) the wrong flow after a mid-flight doc switch. Best-effort: the edit
// itself already committed. It is debounced per flow (REANALYZE_DEBOUNCE_MS):
// holding a move key used to re-analyse the whole flow once per keypress.
export function refreshAfterBlockEdit(updated: FlowDocument) {
  const st = useFlowStore.getState()
  if (st.document?.id !== updated.id) return
  st.applyRemoteDocumentUpdate(updated)

  // Multi-select + inline-rename targets may reference removed blocks.
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

// ---- Per-flow edit lane ----
//
// Block edits (move/duplicate/delete/rename, drag-and-drop) each return the
// WHOLE document, and applying one re-renders the canvas. They used to fire
// one request per keypress with nothing coalesced, responses applied in
// arrival order, and each applied response re-analysed the flow. A lane per
// flow now keeps ONE edit in flight; later edits wait their turn (FIFO,
// bounded by MAX_QUEUED_EDITS), every edit carries a per-flow sequence
// number, and a response is applied only when nothing newer is queued — so a
// burst applies the LAST document once. A failed later edit still applies
// the newest successful response, so the canvas never lags the server.

interface EditLane {
  /** An edit is in flight. */
  busy: boolean
  /** Starters of the edits waiting behind the in-flight one, in order. */
  queue: Array<() => void>
  /** Sequence of the newest edit handed out. */
  issued: number
  /** Sequence of the newest response applied to the stores. */
  applied: number
  /** Newest successful response not applied yet (a newer edit was queued). */
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

/**
 * Runs a block edit through the flow's lane and applies its document via
 * refreshAfterBlockEdit before resolving. Resolves with the edit's response
 * (or undefined when the edit was dropped because the lane's queue was full);
 * rejects with the edit's error, which the caller surfaces as before.
 */
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
      // Every branch below settles the caller's promise; nothing can escape.
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
          // Apply only when nothing newer is queued, then hand the lane on —
          // both before the caller resumes, so it observes the applied state.
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
