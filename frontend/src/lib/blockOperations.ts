import {useCallback} from 'react'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useToast, useConfirm} from '@/components/shared'
import {refreshAfterBlockEdit, runBlockEdit} from '@/lib/blockEdit'
import type {FlowDocument} from '@/types'

// useBlockOperations is the ONE implementation of block mutations:
// move/duplicate/delete/rename with their confirm + per-action-undo toasts
// and the same-flow refresh. Extracted from BlockCard so the canvas keyboard
// shortcuts and the multi-select bulk bar run the exact same code path as
// the context menu. Every mutation goes through the flow's edit lane
// (runBlockEdit): one request in flight per flow, a burst applies the last
// document once, re-analysis debounced.
const refresh = refreshAfterBlockEdit

// capturePreMutationSnapshot reads the NEWEST ring entry BEFORE the mutation
// runs: the server snapshots the pre-state as part of the write, and
// capturing up front is immune to the concurrent-mutation race — reading the
// newest entry AFTER two near-simultaneous mutations could label the wrong
// pre-state as this action's undo target. It runs inside the lane, right
// before its own mutation, so a queued edit captures the state it changes.
async function capturePreMutationSnapshot(flowId: string): Promise<string | undefined> {
  try {
    const snaps = await flowApi.listSnapshots(flowId)
    return snaps?.snapshots?.[snaps.snapshots.length - 1]?.id
  } catch {
    return undefined
  }
}

export function useBlockOperations() {
  const toast = useToast()
  const {confirm} = useConfirm()

  const undoTo = useCallback(
    async (snapshotId?: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      try {
        let id = snapshotId
        if (!id) {
          const snaps = await flowApi.listSnapshots(doc.id)
          id = snaps?.snapshots?.[snaps.snapshots.length - 1]?.id
        }
        if (!id) return
        const res = await flowApi.restoreSnapshot(doc.id, id)
        if (res?.document) refresh(res.document)
      } catch (e) {
        toast.error('Undo failed', {description: String(e)})
      }
    },
    [toast],
  )

  const withUndoToast = useCallback(
    async (undoId: string | undefined, title: string) => {
      toast.success(title, {action: {label: 'Undo', onClick: () => void undoTo(undoId)}})
    },
    [toast, undoTo],
  )

  // lanedMutation runs one undoable mutation through the flow's edit lane:
  // the pre-mutation snapshot is captured inside the lane (immediately before
  // the mutation, so a queued edit captures the state it changes), the
  // applied document is refreshed by runBlockEdit itself, and the undo toast
  // lands only when the edit produced one.
  const lanedMutation = useCallback(
    async <T extends {document?: FlowDocument}>(
      flowId: string,
      op: () => Promise<T>,
      undoTitle: string,
      errTitle: string,
      onSuccess?: () => void,
    ) => {
      try {
        const res = await runBlockEdit(flowId, async () => {
          const preSnap = await capturePreMutationSnapshot(flowId)
          return {...(await op()), preSnap}
        })
        if (res?.document) {
          onSuccess?.()
          await withUndoToast(res.preSnap, undoTitle)
        }
      } catch (e) {
        toast.error(errTitle, {description: String(e)})
      }
    },
    [toast, withUndoToast],
  )

  const moveBlock = useCallback(
    async (blockId: string, direction: 'up' | 'down') => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(
        doc.id,
        () => flowApi.moveBlock(doc.id, blockId, direction),
        direction === 'up' ? 'Block moved up' : 'Block moved down',
        'Move failed',
      )
    },
    [lanedMutation],
  )

  const duplicateBlock = useCallback(
    async (blockId: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(doc.id, () => flowApi.duplicateBlock(doc.id, blockId), 'Block duplicated', 'Duplicate failed')
    },
    [lanedMutation],
  )

  const removeBlock = useCallback(
    async (blockId: string, name: string, childCount = 0) => {
      const ok = await confirm({
        title: 'Delete block',
        message: `Delete "${name}"${childCount > 0 ? ` and its ${childCount} nested block${childCount !== 1 ? 's' : ''}` : ''}? You can undo from the toast.`,
        danger: true,
        confirmLabel: 'Delete',
      })
      if (!ok) return
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(doc.id, () => flowApi.removeBlock(doc.id, blockId), 'Block deleted', 'Delete failed')
    },
    [confirm, lanedMutation],
  )

  const removeBlocks = useCallback(
    async (blockIds: string[]) => {
      if (blockIds.length === 0) return
      const ok = await confirm({
        title: `Delete ${blockIds.length} blocks`,
        message: `Delete ${blockIds.length} selected blocks? Nested blocks inside them go too. You can undo from the toast.`,
        danger: true,
        confirmLabel: 'Delete',
      })
      if (!ok) return
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(
        doc.id,
        () => flowApi.removeBlocks(doc.id, blockIds),
        `${blockIds.length} blocks deleted`,
        'Bulk delete failed',
        () => useFlowStore.getState().clearBlockSelection(),
      )
    },
    [confirm, lanedMutation],
  )

  const renameBlock = useCallback(
    async (blockId: string, name: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      try {
        const res = await runBlockEdit(doc.id, () => flowApi.renameBlock(doc.id, blockId, name))
        if (res?.document) {
          useFlowStore.getState().setRenamingBlock(null)
          const refs = res.gotoRefsUpdated ?? 0
          toast.success(
            'Renamed',
            refs > 0 ? {description: `${refs} GOTO reference${refs !== 1 ? 's' : ''} updated`} : undefined,
          )
        }
      } catch (e) {
        toast.error('Rename failed', {description: String(e)})
      }
    },
    [toast],
  )

  return {moveBlock, duplicateBlock, removeBlock, removeBlocks, renameBlock, undoTo}
}
