import {useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useToast, useConfirm} from '@/components/shared'
import {refreshAfterBlockEdit, runBlockEdit} from '@/lib/blockEdit'
import type {FlowDocument} from '@/types'
import {getErrorMessage} from '@/lib/getErrorMessage'

const refresh = refreshAfterBlockEdit

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
  const {t} = useTranslation('flow')

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
        toast.error(t('blockOps.undoFailed'), {description: getErrorMessage(e)})
      }
    },
    [toast, t],
  )

  const withUndoToast = useCallback(
    async (undoId: string | undefined, title: string) => {
      toast.success(title, {action: {label: t('blockOps.undo'), onClick: () => void undoTo(undoId)}})
    },
    [toast, t, undoTo],
  )

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
        toast.error(errTitle, {description: getErrorMessage(e)})
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
        direction === 'up' ? t('blockOps.movedUp') : t('blockOps.movedDown'),
        t('blockOps.moveFailed'),
      )
    },
    [lanedMutation, t],
  )

  const duplicateBlock = useCallback(
    async (blockId: string) => {
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(
        doc.id,
        () => flowApi.duplicateBlock(doc.id, blockId),
        t('blockOps.duplicated'),
        t('blockOps.duplicateFailed'),
      )
    },
    [lanedMutation, t],
  )

  const removeBlock = useCallback(
    async (blockId: string, name: string, childCount = 0) => {
      const ok = await confirm({
        title: t('blockOps.deleteConfirmTitle'),
        message:
          childCount > 0
            ? t('blockOps.deleteConfirmNested', {name, count: childCount})
            : t('blockOps.deleteConfirmMessage', {name}),
        danger: true,
        confirmLabel: t('blockOps.deleteConfirmLabel'),
      })
      if (!ok) return
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(
        doc.id,
        () => flowApi.removeBlock(doc.id, blockId),
        t('blockOps.deleted'),
        t('blockOps.deleteFailed'),
      )
    },
    [confirm, lanedMutation, t],
  )

  const removeBlocks = useCallback(
    async (blockIds: string[]) => {
      if (blockIds.length === 0) return
      const ok = await confirm({
        title: t('blockOps.bulkDeleteTitle', {count: blockIds.length}),
        message: t('blockOps.bulkDeleteMessage', {count: blockIds.length}),
        danger: true,
        confirmLabel: t('blockOps.deleteConfirmLabel'),
      })
      if (!ok) return
      const doc = useFlowStore.getState().document
      if (!doc) return
      await lanedMutation(
        doc.id,
        () => flowApi.removeBlocks(doc.id, blockIds),
        t('blockOps.bulkDeleted', {count: blockIds.length}),
        t('blockOps.bulkDeleteFailed'),
        () => useFlowStore.getState().clearBlockSelection(),
      )
    },
    [confirm, lanedMutation, t],
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
            t('blockOps.renamed'),
            refs > 0 ? {description: t('blockOps.renameRefsUpdated', {count: refs})} : undefined,
          )
        }
      } catch (e) {
        toast.error(t('blockOps.renameFailed'), {description: getErrorMessage(e)})
      }
    },
    [toast, t],
  )

  return {moveBlock, duplicateBlock, removeBlock, removeBlocks, renameBlock, undoTo}
}
