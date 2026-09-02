import {useMemo, useState, useCallback} from 'react'
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
      if (values[p.key] !== p.value) out[p.key] = values[p.key]
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
        toast.success(`Updated ${Object.keys(changes).length} propert${Object.keys(changes).length === 1 ? 'y' : 'ies'}`, {
          action: {
            label: 'Undo',
            onClick: () => {
              void (async () => {
                try {
                  const r = await flowApi.restoreSnapshot(doc.id, undoId)
                  if (r?.document) refresh(r.document)
                } catch (e) {
                  toast.error('Undo failed', {description: String(e)})
                }
              })()
            },
          },
        })
      } else {
        toast.success(`Updated ${Object.keys(changes).length} propert${Object.keys(changes).length === 1 ? 'y' : 'ies'}`)
      }
      onClose()
    } catch (e) {
      toast.error('Update failed', {description: String(e)})
    } finally {
      setSaving(false)
    }
  }, [dirty, saving, block.id, changes, refresh, toast, onClose])

  return (
    <Modal
      isOpen
      onClose={onClose}
      title="Edit properties"
      ariaLabel={`Edit properties: ${block.name}`}
      size="md"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" size="sm" disabled={!dirty || saving} onClick={() => void handleSave()}>
            {saving ? 'Saving…' : dirty ? `Save ${Object.keys(changes).length} change${Object.keys(changes).length === 1 ? '' : 's'}` : 'Save'}
          </Button>
        </>
      }
    >
      <p className="text-2xs text-text-tertiary truncate -mt-1 mb-2">{block.name}</p>
      <div className="space-y-2.5 max-h-[50vh] overflow-y-auto">
        {editable.length === 0 ? (
          <p className="text-xs text-text-tertiary">This block has no editable properties.</p>
        ) : (
          editable.map(p => (
            <label key={p.key} className="block">
              <span className="text-2xs font-medium text-text-secondary font-mono">{p.key}</span>
              <input
                value={values[p.key] ?? ''}
                onChange={e => setValues(v => ({...v, [p.key]: e.target.value}))}
                className="mt-0.5 w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary outline-none focus:border-brand-500 font-mono"
                aria-label={`Value for ${p.key}`}
              />
            </label>
          ))
        )}
      </div>
    </Modal>
  )
}
