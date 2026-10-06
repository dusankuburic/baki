import {useMemo, useState, useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {} from 'lucide-react'
import {Button, Modal} from '@/components/shared'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useToast} from '@/components/shared'
import type {Block} from '@/types'
import {refreshAfterBlockEdit} from '@/lib/blockEdit'

interface Props {
  block: Block
  onClose: () => void
}

// editableProperties filters the block's properties to the editable surface:
// parser-derived `_`-prefixed keys (e.g. _output, _var, _retry*) are
// structural, not user-written text.
function editableProperties(block: Block): {key: string; value: string}[] {
  return Object.entries(block.properties ?? {})
    .filter(([k]) => !k.startsWith('_'))
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, value]) => ({key, value}))
}

// BlockPropertiesModal is the structured property editor (R3-2): one input
// per editable property (parsed values, unquoted), a live diff on save —
// only CHANGED keys are sent as targeted server-side replaces, so untouched
// properties keep their original text, order, and quoting.
export default function BlockPropertiesModal({block, onClose}: Props) {
  const {t} = useTranslation('flow')
  const toast = useToast()
  const editable = useMemo(() => editableProperties(block), [block])
  const [values, setValues] = useState<Record<string, string>>(() => {
    const init: Record<string, string> = {}
    for (const p of editable) init[p.key] = p.value
    return init
  })
  const [saving, setSaving] = useState(false)

  const changes = useMemo(() => {
    const out: Record<string, string> = {}
    for (const p of editable) {
      // values is seeded from editable above, so every p.key is present.
      const v = values[p.key]
      if (v !== undefined && v !== p.value) out[p.key] = v
    }
    return out
  }, [editable, values])
  const dirty = Object.keys(changes).length > 0

  const refresh = refreshAfterBlockEdit

  const handleSave = useCallback(async () => {
    if (!dirty || saving) return
    const doc = useFlowStore.getState().document
    if (!doc) return
    // Pre-state capture BEFORE the write (F1.6 — race-safe undo targeting).
    let undoId: string | undefined
    try {
      const snaps = await flowApi.listSnapshots(doc.id)
      undoId = snaps?.snapshots?.[snaps.snapshots.length - 1]?.id
    } catch {
      /* undo is best-effort */
    }
    setSaving(true)
    try {
      const res = await flowApi.updateBlockProperties(doc.id, block.id, changes)
      if (res?.document) refresh(res.document)
      if (undoId) {
        toast.success(t('props.updated', {count: Object.keys(changes).length}), {
          action: {
            label: t('props.undo'),
            onClick: () => {
              void (async () => {
                try {
                  const r = await flowApi.restoreSnapshot(doc.id, undoId)
                  if (r?.document) refresh(r.document)
                } catch (e) {
                  toast.error(t('props.undoFailed'), {description: String(e)})
                }
              })()
            },
          },
        })
      } else {
        toast.success(t('props.updated', {count: Object.keys(changes).length}))
      }
      onClose()
    } catch (e) {
      toast.error(t('props.updateFailed'), {description: String(e)})
    } finally {
      setSaving(false)
    }
  }, [dirty, saving, block.id, changes, refresh, toast, onClose, t])

  return (
    <Modal
      isOpen
      onClose={onClose}
      title={t('props.title')}
      ariaLabel={t('props.ariaLabel', {name: block.name})}
      size="md"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t('props.cancel')}
          </Button>
          <Button variant="primary" size="sm" disabled={!dirty || saving} onClick={() => void handleSave()}>
            {saving
              ? t('props.saving')
              : dirty
                ? t('props.saveChanges', {count: Object.keys(changes).length})
                : t('props.save')}
          </Button>
        </>
      }
    >
      <p className="text-2xs text-text-tertiary truncate -mt-1 mb-2">{block.name}</p>
      <div className="space-y-2.5 max-h-[50vh] overflow-y-auto">
        {editable.length === 0 ? (
          <p className="text-xs text-text-tertiary">{t('props.empty')}</p>
        ) : (
          editable.map(p => (
            <label key={p.key} className="block">
              <span className="text-2xs font-medium text-text-secondary font-mono">{p.key}</span>
              <input
                value={values[p.key] ?? ''}
                onChange={e => setValues(v => ({...v, [p.key]: e.target.value}))}
                className="mt-0.5 w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary outline-hidden focus:border-brand-500 font-mono"
                aria-label={t('props.valueAria', {key: p.key})}
              />
            </label>
          ))
        )}
      </div>
    </Modal>
  )
}
