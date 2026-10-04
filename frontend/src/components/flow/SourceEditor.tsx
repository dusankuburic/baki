import {useState, useEffect, useRef, useCallback, memo, type RefObject} from 'react'
import {useTranslation} from 'react-i18next'
import {Save, RotateCcw, Code2, Plus} from 'lucide-react'
import {flowApi, analysisApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useToast, useConfirm, Spinner} from '@/components/shared'
import {countLines, lineCountAfterEdit, visibleLineRange} from '@/lib/textLines'
import type {AnalysisReport} from '@/types'
import AddActionForm, {insertBeforeLastRegionEnd} from './AddActionForm'

interface SourceEditorProps {
  onClose: () => void
}

// Lines rendered beyond the viewport on either side, so a scroll step never
// shows a blank gutter before the next render.
const GUTTER_BUFFER_LINES = 20
// `leading-5` (textarea and gutter alike) until the textarea has been measured.
const FALLBACK_LINE_HEIGHT_PX = 20
// Viewport height until the textarea has been laid out (and in jsdom).
const FALLBACK_VIEWPORT_PX = 800

/**
 * Line-number gutter rendering only the lines inside the textarea's viewport
 * (plus a buffer either side), with spacers keeping the scroll geometry equal
 * to the textarea's. One <div> per line meant 30k nodes for real PAD exports,
 * and a single added line re-rendered all of them (369 ms). The textarea owns
 * scrolling; the gutter mirrors its scrollTop and re-renders only when the
 * viewport crosses a line boundary.
 */
const LineGutter = memo(function LineGutter({
  lineCount,
  textareaRef,
}: {
  lineCount: number
  textareaRef: RefObject<HTMLTextAreaElement>
}) {
  const gutterRef = useRef<HTMLDivElement>(null)
  const [scrollLine, setScrollLine] = useState(0)
  const [metrics, setMetrics] = useState({height: FALLBACK_VIEWPORT_PX, lineHeight: FALLBACK_LINE_HEIGHT_PX})
  const {lineHeight} = metrics

  useEffect(() => {
    const el = textareaRef.current
    if (!el) return
    const onScroll = () => {
      if (gutterRef.current) gutterRef.current.scrollTop = el.scrollTop
      setScrollLine(Math.floor(el.scrollTop / lineHeight))
    }
    el.addEventListener('scroll', onScroll, {passive: true})
    // ResizeObserver notifies once on observe, which is when the textarea is
    // first measured; until then (and where it is unavailable) the fallbacks
    // render a generous window.
    let observer: ResizeObserver | null = null
    if (typeof ResizeObserver !== 'undefined') {
      observer = new ResizeObserver(() => {
        const measured = parseFloat(getComputedStyle(el).lineHeight)
        const next = {
          height: el.clientHeight || FALLBACK_VIEWPORT_PX,
          lineHeight: Number.isFinite(measured) && measured > 0 ? measured : FALLBACK_LINE_HEIGHT_PX,
        }
        setMetrics(prev => (prev.height === next.height && prev.lineHeight === next.lineHeight ? prev : next))
      })
      observer.observe(el)
    }
    return () => {
      el.removeEventListener('scroll', onScroll)
      observer?.disconnect()
    }
  }, [textareaRef, lineHeight])

  const {first, end} = visibleLineRange(
    scrollLine * lineHeight,
    metrics.height,
    lineHeight,
    lineCount,
    GUTTER_BUFFER_LINES,
  )
  const numbers = []
  for (let i = first; i < end; i++) {
    numbers.push(
      <div key={i} className="leading-5">
        {i + 1}
      </div>,
    )
  }

  return (
    <div
      ref={gutterRef}
      data-testid="line-gutter"
      className="flex-shrink-0 overflow-hidden py-2 pr-2 pl-3 text-right text-text-tertiary select-none bg-surface-2 border-r border-border-subtle"
      // Sized for the widest number so the width does not jump while scrolling
      // (only the visible numbers are in the DOM).
      style={{minWidth: `max(3rem, calc(${String(lineCount).length}ch + 1.5rem))`}}
    >
      <div aria-hidden style={{height: first * lineHeight}} />
      {numbers}
      <div aria-hidden style={{height: (lineCount - end) * lineHeight}} />
    </div>
  )
})

// SourceEditor is a lightweight in-app PAD source editor: a monospace textarea
// with synced line numbers, "Save & Re-parse" (round-trips through the backend
// which re-parses + persists), and "Revert". No external editor dependency —
// the textarea + line-number gutter is sufficient for PAD text, which has no
// standard syntax-highlighting grammar.
// memberFileName returns the bare filename the editor targets for a desktop
// FOLDER flow: the file selected in the sidebar, else Main.txt (the server
// default). Single-file and cloud flows have no member to pick.
function memberFileName(document: {isFolder?: boolean; filePath: string} | null, selectedFilePath: string | null) {
  if (!document?.isFolder || !document.filePath) return undefined
  const selected = selectedFilePath?.split(/[/\\]/).pop()
  return selected || 'Main.txt'
}

export default function SourceEditor({onClose}: SourceEditorProps) {
  const document = useFlowStore(s => s.document)
  const selectedFilePath = useFlowStore(s => s.selectedFilePath)
  // Folder flows: read and write the SELECTED member file. The editor used to
  // load Main.txt unconditionally and label nothing, so editing "Subflow.txt"
  // (selected in the sidebar) overwrote Main.
  const requestedFile = memberFileName(document, selectedFilePath)
  const setReport = useAnalysisStore(s => s.setReport)
  const toast = useToast()
  const {confirm} = useConfirm()
  const {t} = useTranslation('shell')

  // The buffer carries its line count: each edit adjusts it by scanning only
  // the changed region (lineCountAfterEdit) instead of re-splitting the
  // whole source per keystroke.
  const [buffer, setBuffer] = useState<{text: string; lines: number}>({text: '', lines: 1})
  const source = buffer.text
  const lineCount = buffer.lines
  const setSource = useCallback((next: string | ((prev: string) => string)) => {
    setBuffer(prev => {
      const text = typeof next === 'function' ? next(prev.text) : next
      if (text === prev.text) return prev
      return {text, lines: prev.text === '' ? countLines(text) : lineCountAfterEdit(prev.text, text, prev.lines)}
    })
  }, [])
  const [original, setOriginal] = useState('')
  // memberFile is the folder member whose source is IN the buffer. It follows
  // the sidebar selection only while the buffer is clean: switching on a
  // selection change with unsaved edits would refetch and overwrite them. The
  // header shows memberFile, so the user sees which file a Save will write.
  const [memberFile, setMemberFile] = useState(requestedFile)
  if (memberFile !== requestedFile && source === original) {
    setMemberFile(requestedFile)
  }
  const [addingAction, setAddingAction] = useState(false)
  const readOnly = useFlowStore(s => s.readOnly)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  // loadError is what stops a FAILED load from presenting as an EMPTY flow.
  // Without it the catch below only toasted, leaving source and original at
  // '' while loading cleared — so the editor rendered a fully interactive
  // blank textarea. One keystroke made it dirty (original was ''), which armed
  // Save, which sent that single character as the entire PAD source. A
  // transient 500 on load therefore destroyed the flow.
  const [loadError, setLoadError] = useState<string | null>(null)
  const [reloadNonce, setReloadNonce] = useState(0)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Load the raw source on mount.
  useEffect(() => {
    if (!document) return
    let cancelled = false
    // Enters the loading state for an async source fetch.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void setLoading(true)
    void setLoadError(null)
    flowApi
      .getSource(document.id, memberFile)
      .then(res => {
        if (cancelled) return
        setSource(res.source)
        setOriginal(res.source)
      })
      .catch(err => {
        if (!cancelled) {
          setLoadError(String(err))
          toast.error('Failed to load source', {description: String(err)})
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // Keyed on document IDENTITY (and the selected member file) only.
    // `document` is replaced on every save/reparse, and re-running this would
    // re-fetch and overwrite the user's unsaved edits; `toast` is only used on
    // the failure path.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [document?.id, memberFile, reloadNonce])

  const dirty = source !== original
  // The toolbar's view switcher can close the editor too; it reads this flag
  // to confirm before discarding edits (see MainPaneToolbar).
  const setSourceDirty = useUIStore(s => s.setSourceEditorDirty)
  useEffect(() => {
    setSourceDirty(dirty)
    return () => setSourceDirty(false)
  }, [dirty, setSourceDirty])
  // Cloud folder flows (no on-disk path) store no per-file source: the server
  // refuses to save the combined text because it would collapse the folder
  // structure, so Save used to arm on the first keystroke and always fail.
  // Desktop folder flows save the selected member file and stay enabled.
  const cloudFolder = !!document?.isFolder && !document.filePath

  const handleSave = useCallback(async () => {
    // readOnly guards the mod+S shortcut too — the button was already
    // disabled, the keyboard path wasn't.
    // loadError: the buffer does not hold this flow's source, it holds
    // whatever was typed into an empty box. Saving it would overwrite the real
    // flow with that.
    if (!document || !dirty || saving || loadError || cloudFolder || useFlowStore.getState().readOnly) return
    setSaving(true)
    try {
      const updated = await flowApi.saveSource(document.id, source, memberFile)
      // Same-flow refresh: preserve subflow/chat/selection.
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
  }, [document, memberFile, source, dirty, saving, loadError, cloudFolder, setOriginal, setReport, toast])

  const handleRevert = useCallback(() => {
    setSource(original)
    textareaRef.current?.focus()
  }, [original, setSource])

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
    [setSource],
  )

  // Guard: confirm before discarding unsaved edits when switching to Block view.
  const handleClose = useCallback(async () => {
    if (dirty) {
      const ok = await confirm({
        title: t('mainPane.discardSourceTitle'),
        message: t('mainPane.discardSourceMessage'),
        danger: true,
        confirmLabel: t('mainPane.discardSourceConfirm'),
      })
      if (!ok) return
    }
    onClose()
  }, [dirty, confirm, onClose, t])

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

  if (loading) {
    return (
      <div className="flex items-center justify-center h-full">
        <Spinner size={24} />
      </div>
    )
  }

  if (loadError) {
    return (
      <div className="flex flex-col items-center justify-center h-full gap-3 p-6 text-center">
        <div className="text-sm font-medium text-text-primary">Couldn&apos;t load the flow source</div>
        <div className="max-w-md text-xs text-text-tertiary break-words">{loadError}</div>
        <div className="max-w-md text-xs text-text-tertiary">
          The editor stays closed so a save can&apos;t overwrite the flow with an empty buffer.
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setReloadNonce(n => n + 1)}
            className="px-3 py-1.5 text-xs rounded bg-surface-3 text-text-primary hover:bg-surface-2"
          >
            Retry
          </button>
          <button onClick={onClose} className="px-3 py-1.5 text-xs rounded text-text-tertiary hover:text-text-primary">
            Close
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full bg-surface-1">
      {/* Toolbar */}
      <div className="flex items-center justify-between px-3 py-1.5 border-b border-border-subtle bg-surface-2">
        <div className="flex items-center gap-2 text-xs text-text-tertiary">
          <Code2 size={14} />
          <span className="font-medium">Source Editor</span>
          {memberFile && (
            <span className="font-mono text-text-secondary" title="Member file of this flow folder being edited">
              · {memberFile}
            </span>
          )}
          {dirty && <span className="text-semantic-warning">• unsaved</span>}
          {readOnly && <span className="text-2xs text-text-tertiary">· read-only</span>}
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setAddingAction(a => !a)}
            disabled={saving || !!document?.isFolder || readOnly}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
            title={
              document?.isFolder
                ? 'Structured editing is single-file for now'
                : 'Compose a PAD action line and insert it'
            }
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
            disabled={!dirty || saving || readOnly || cloudFolder}
            title={
              cloudFolder
                ? 'Saving a multi-file flow is not supported here (it would collapse the folder structure)'
                : undefined
            }
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

      {addingAction && <AddActionForm onInsert={handleInsertAction} onClose={() => setAddingAction(false)} />}

      {/* Editor: line-number gutter + textarea (the gutter follows the textarea's scroll) */}
      <div className="flex flex-1 overflow-hidden font-mono text-xs">
        <LineGutter lineCount={lineCount} textareaRef={textareaRef} />
        <textarea
          ref={textareaRef}
          value={source}
          onChange={e => setSource(e.target.value)}
          spellCheck={false}
          className="flex-1 bg-surface-1 text-text-primary p-2 outline-none resize-none leading-5 whitespace-pre overflow-auto"
          style={{tabSize: 4}}
        />
      </div>
    </div>
  )
}
