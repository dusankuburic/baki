import {useCallback} from 'react'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useToast, useConfirm} from '@/components/shared'
import {refreshAfterBlockEdit} from '@/lib/blockEdit'

// useBlockOperations is the ONE implementation of block mutations (U3b):
// move/duplicate/delete/rename with their confirm + per-action-undo toasts
// and the same-flow refresh. Extracted from BlockCard so the canvas keyboard
// shortcuts and the multi-select bulk bar run the exact same code path as
// the context menu.
const refresh = refreshAfterBlockEdit

// capturePreMutationSnapshot reads the NEWEST ring entry BEFORE the mutation
// runs (F1.6): the server snapshots the pre-state as part of the write, and
// capturing up front is immune to the concurrent-mutation race — reading the
// newest entry AFTER two near-simultaneous mutations could label the wrong
// pre-state as this action's undo target.
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

  const moveBlock = useCallback(
    async (blockId: string, direction: 'up' | 'down') => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      const preSnap = await capturePreMutationSnapshot(doc.id)
      try {
        const res = await flowApi.moveBlock(doc.id, blockId, direction)
        if (res?.document) {
          refresh(res.document)
          await withUndoToast(preSnap, direction === 'up' ? 'Block moved up' : 'Block moved down')
        }
      } catch (e) {
        toast.error('Move failed', {description: String(e)})
      }
    },
    [toast, withUndoToast],
  )

  const duplicateBlock = useCallback(
    async (blockId: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      const preSnap = await capturePreMutationSnapshot(doc.id)
      try {
        const res = await flowApi.duplicateBlock(doc.id, blockId)
        if (res?.document) {
          refresh(res.document)
          await withUndoToast(preSnap, 'Block duplicated')
        }
      } catch (e) {
        toast.error('Duplicate failed', {description: String(e)})
      }
    },
    [toast, withUndoToast],
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
      const preSnap = await capturePreMutationSnapshot(doc.id)
      try {
        const res = await flowApi.removeBlock(doc.id, blockId)
        if (res?.document) {
          refresh(res.document)
          await withUndoToast(preSnap, 'Block deleted')
        }
      } catch (e) {
        toast.error('Delete failed', {description: String(e)})
      }
    },
    [confirm, toast, withUndoToast],
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
      const preSnap = await capturePreMutationSnapshot(doc.id)
      try {
        const res = await flowApi.removeBlocks(doc.id, blockIds)
        if (res?.document) {
          useFlowStore.getState().clearBlockSelection()
          refresh(res.document)
          await withUndoToast(preSnap, `${blockIds.length} blocks deleted`)
        }
      } catch (e) {
        toast.error('Bulk delete failed', {description: String(e)})
      }
    },
    [confirm, toast, withUndoToast],
  )

  const renameBlock = useCallback(
    async (blockId: string, name: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      try {
        const res = await flowApi.renameBlock(doc.id, blockId, name)
        if (res?.document) {
          useFlowStore.getState().setRenamingBlock(null)
          refresh(res.document)
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
