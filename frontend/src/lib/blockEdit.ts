import {analysisApi} from '@/api'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument} from '@/types'

// refreshAfterBlockEdit pushes a server-confirmed SAME-FLOW mutation into the
// local stores (F1.1/F1.2). Two correctness rules:
//   1. Doc-switch race guard — if the user opened a different flow while the
//      request was in flight, the stale result must NOT replace their doc.
//   2. Same-flow refresh rides applyRemoteDocumentUpdate (NOT setDocument):
//      it preserves the active subflow, chat thread, navigation history, and
//      a still-existing block selection — setDocument reset all of that,
//      teleporting every edit back to subflow #1 and killing the chat.
// Re-analysis targets the UPDATED flow id — analyzeFlow() resolves the
// currently-active doc at call time and could analyze (and store findings
// for) the wrong flow after a mid-flight doc switch. Best-effort: the edit
// itself already committed.
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

  analysisApi
    .analyzeFlowById(updated.id)
    .then(r => {
      if (r) useAnalysisStore.getState().setReport(updated.id, r)
    })
    .catch(() => {})
}

function collectBlockIds(blocks: import('@/types').Block[], out: Set<string>) {
  for (const b of blocks) {
    out.add(b.id)
    if (b.children?.length) collectBlockIds(b.children, out)
  }
}
