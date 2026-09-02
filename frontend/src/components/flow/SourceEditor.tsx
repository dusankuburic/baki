import {useState, useEffect, useRef, useCallback, memo} from 'react'
import {Save, RotateCcw, Code2, Plus} from 'lucide-react'
import {flowApi, analysisApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useToast, useConfirm, Spinner} from '@/components/shared'
import type {AnalysisReport} from '@/types'
import AddActionForm, {insertBeforeLastRegionEnd} from './AddActionForm'

interface SourceEditorProps {
  onClose: () => void
}

/**
 * Line-number gutter, memoized on the LINE COUNT only: typing re-splits the
 * source and re-rendered one <div> per line (thousands on real PAD exports)
 * per keystroke; now the gutter only re-renders when a line is added/removed.
 */
const LineGutter = memo(function LineGutter({lineCount}: {lineCount: number}) {
  return (
    <>
      {Array.from({length: lineCount}, (_, i) => (
        <div key={i} className="leading-5">
          {i + 1}
        </div>
      ))}
    </>
  )
})

// SourceEditor is a lightweight in-app PAD source editor: a monospace textarea
// with synced line numbers, "Save & Re-parse" (round-trips through the backend
// which re-parses + persists), and "Revert". No external editor dependency —
// the textarea + line-number gutter is sufficient for PAD text, which has no
// standard syntax-highlighting grammar.
export default function SourceEditor({onClose}: SourceEditorProps) {
  const document = useFlowStore(s => s.document)
  const setDocument = useFlowStore(s => s.setDocument)
  const setReport = useAnalysisStore(s => s.setReport)
  const toast = useToast()
  const {confirm} = useConfirm()

  const [source, setSource] = useState('')
  const [original, setOriginal] = useState('')
  const [addingAction, setAddingAction] = useState(false)
  const readOnly = useFlowStore(s => s.readOnly)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const gutterRef = useRef<HTMLDivElement>(null)

  // Load the raw source on mount.
  useEffect(() => {
    if (!document) return
    let cancelled = false
    void setLoading(true)
    flowApi
      .getSource(document.id)
      .then(res => {
        if (cancelled) return
        setSource(res.source)
        setOriginal(res.source)
      })
      .catch(err => {
        if (!cancelled) toast.error('Failed to load source', {description: String(err)})
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [document?.id])

  const dirty = source !== original

  const handleSave = useCallback(async () => {
    // readOnly guards the mod+S shortcut too (F1.5) — the button was already
    // disabled, the keyboard path wasn't.
    if (!document || !dirty || saving || useFlowStore.getState().readOnly) return
    setSaving(true)
    try {
      const updated = await flowApi.saveSource(document.id, source)
      // Same-flow refresh (F1.1): preserve subflow/chat/selection.
      useFlowStore.getState().applyRemoteDocumentUpdate(updated)
      setOriginal(source)
      // Best-effort re-analysis so findings reflect the edited source —
      // targeted at the SAVED flow, not whatever is active when it lands.
      try {
        const r = await analysisApi.analyzeFlowById(updated.id)
        if (r) setReport(updated.id, r as AnalysisReport)
      } catch {
        /* re-analysis is best-effort */
      }
      toast.success('Source saved', {description: 'Flow re-parsed successfully.'})
    } catch (err) {
      toast.error('Save failed', {description: String(err)})
    } finally {
      setSaving(false)
    }
  }, [document, source, dirty, saving, setDocument, setOriginal, setReport, toast])

  const handleRevert = useCallback(() => {
    setSource(original)
    textareaRef.current?.focus()
  }, [original])

  // handleInsertAction appends a composed action line to the end of the LAST
  // subflow region (the structured-editing slice): users who don't know PAD's
  // text format still build flows; power users see the raw line in the
  // editor before saving.
  const handleInsertAction = useCallback(
    (line: string) => {
      setSource(prev => insertBeforeLastRegionEnd(prev, line))
      setAddingAction(false)
      textareaRef.current?.focus()
    },
    [],
  )

  // Guard: confirm before discarding unsaved edits when switching to Block view.
  const handleClose = useCallback(async () => {
    if (dirty) {
      const ok = await confirm({
        title: 'Discard unsaved changes?',
        message: 'You have unsaved edits in the source editor. Switching to Block view will discard them.',
        danger: true,
        confirmLabel: 'Discard',
      })
      if (!ok) return
    }
    onClose()
  }, [dirty, confirm, onClose])

  // Ctrl+S / Cmd+S keyboard shortcut for save.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 's') {
        e.preventDefault()
        void handleSave()
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [handleSave])

  // Sync line-number gutter scroll with the textarea.
  const handleScroll = useCallback(() => {
    if (gutterRef.current && textareaRef.current) {
      gutterRef.current.scrollTop = textareaRef.current.scrollTop
    }
  }, [])

  if (loading) {
    return (
      <div className="flex items-center justify-center h-full">
        <Spinner size={24} />
      </div>
    )
  }

  const lineCount = source.split('\n').length

  return (
    <div className="flex flex-col h-full bg-surface-1">
      {/* Toolbar */}
      <div className="flex items-center justify-between px-3 py-1.5 border-b border-border-subtle bg-surface-2">
        <div className="flex items-center gap-2 text-xs text-text-tertiary">
          <Code2 size={14} />
          <span className="font-medium">Source Editor</span>
          {dirty && <span className="text-semantic-warning">• unsaved</span>}
          {readOnly && <span className="text-2xs text-text-tertiary">· read-only</span>}
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setAddingAction(a => !a)}
            disabled={saving || !!document?.isFolder || readOnly}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
            title={document?.isFolder ? 'Structured editing is single-file for now' : 'Compose a PAD action line and insert it'}
          >
            <Plus size={12} />
            Add action
          </button>
          <button
            onClick={handleRevert}
            disabled={!dirty || saving}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
          >
            <RotateCcw size={12} />
            Revert
          </button>
          <button
            onClick={handleSave}
            disabled={!dirty || saving || readOnly}
            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
          >
            {saving ? <Spinner size={12} /> : <Save size={12} />}
            {saving ? 'Saving…' : 'Save & Re-parse'}
          </button>
          <button
            onClick={handleClose}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 transition-colors"
          >
            Block view
          </button>
        </div>
      </div>

      {addingAction && (
        <AddActionForm onInsert={handleInsertAction} onClose={() => setAddingAction(false)} />
      )}

      {/* Editor: line-number gutter + textarea */}
      <div className="flex flex-1 overflow-hidden font-mono text-xs">
        <div
          ref={gutterRef}
          className="flex-shrink-0 overflow-hidden py-2 pr-2 pl-3 text-right text-text-tertiary select-none bg-surface-2 border-r border-border-subtle"
          style={{minWidth: '3rem'}}
        >
          <LineGutter lineCount={lineCount} />
        </div>
        <textarea
          ref={textareaRef}
          value={source}
          onChange={e => setSource(e.target.value)}
          onScroll={handleScroll}
          spellCheck={false}
          className="flex-1 bg-surface-1 text-text-primary p-2 outline-none resize-none leading-5 whitespace-pre overflow-auto"
          style={{tabSize: 4}}
        />
      </div>
    </div>
  )
}
