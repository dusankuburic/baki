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
import {getErrorMessage} from '@/lib/getErrorMessage'

interface SourceEditorProps {
  onClose: () => void
}

const GUTTER_BUFFER_LINES = 20
const FALLBACK_LINE_HEIGHT_PX = 20
const FALLBACK_VIEWPORT_PX = 800

const LineGutter = memo(function LineGutter({
  lineCount,
  textareaRef,
}: {
  lineCount: number
  textareaRef: RefObject<HTMLTextAreaElement | null>
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
      className="shrink-0 overflow-hidden py-2 pr-2 pl-3 text-right text-text-tertiary select-none bg-surface-2 border-r border-border-subtle"
      style={{minWidth: `max(3rem, calc(${String(lineCount).length}ch + 1.5rem))`}}
    >
      <div aria-hidden style={{height: first * lineHeight}} />
      {numbers}
      <div aria-hidden style={{height: (lineCount - end) * lineHeight}} />
    </div>
  )
})

function memberFileName(document: {isFolder?: boolean; filePath: string} | null, selectedFilePath: string | null) {
  if (!document?.isFolder || !document.filePath) return undefined
  const selected = selectedFilePath?.split(/[/\\]/).pop()
  return selected || 'Main.txt'
}

export default function SourceEditor({onClose}: SourceEditorProps) {
  const document = useFlowStore(s => s.document)
  const selectedFilePath = useFlowStore(s => s.selectedFilePath)
  const requestedFile = memberFileName(document, selectedFilePath)
  const setReport = useAnalysisStore(s => s.setReport)
  const toast = useToast()
  const {confirm} = useConfirm()
  const {t} = useTranslation('shell')
  const {t: tf} = useTranslation('flow')

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
  const [memberFile, setMemberFile] = useState(requestedFile)
  if (memberFile !== requestedFile && source === original) {
    setMemberFile(requestedFile)
  }
  const [addingAction, setAddingAction] = useState(false)
  const readOnly = useFlowStore(s => s.readOnly)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [reloadNonce, setReloadNonce] = useState(0)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    if (!document) return
    let cancelled = false
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
          setLoadError(getErrorMessage(err))
          toast.error(tf('source.loadFailed'), {description: getErrorMessage(err)})
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [document?.id, memberFile, reloadNonce])

  const dirty = source !== original
  const setSourceDirty = useUIStore(s => s.setSourceEditorDirty)
  useEffect(() => {
    setSourceDirty(dirty)
    return () => setSourceDirty(false)
  }, [dirty, setSourceDirty])
  const cloudFolder = !!document?.isFolder && !document.filePath

  const handleSave = useCallback(async () => {
    if (!document || !dirty || saving || loadError || cloudFolder || useFlowStore.getState().readOnly) return
    setSaving(true)
    try {
      const updated = await flowApi.saveSource(document.id, source, memberFile)
      useFlowStore.getState().applyRemoteDocumentUpdate(updated)
      setOriginal(source)
      try {
        const r = await analysisApi.analyzeFlowById(updated.id)
        if (r) setReport(updated.id, r as AnalysisReport)
      } catch {
      }
      toast.success(tf('source.saved'), {description: tf('source.savedBody')})
    } catch (err) {
      toast.error(tf('source.saveFailed'), {description: getErrorMessage(err)})
    } finally {
      setSaving(false)
    }
  }, [document, memberFile, source, dirty, saving, loadError, cloudFolder, setOriginal, setReport, toast, tf])

  const handleRevert = useCallback(() => {
    setSource(original)
    textareaRef.current?.focus()
  }, [original, setSource])

  const handleInsertAction = useCallback(
    (line: string) => {
      setSource(prev => insertBeforeLastRegionEnd(prev, line))
      setAddingAction(false)
      textareaRef.current?.focus()
    },
    [setSource],
  )

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
        <div className="text-sm font-medium text-text-primary">{tf('source.errorTitle')}</div>
        <div className="max-w-md text-xs text-text-tertiary wrap-break-word">{loadError}</div>
        <div className="max-w-md text-xs text-text-tertiary">{tf('source.errorBody')}</div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setReloadNonce(n => n + 1)}
            className="px-3 py-1.5 text-xs rounded bg-surface-3 text-text-primary hover:bg-surface-2"
          >
            {tf('source.retry')}
          </button>
          <button onClick={onClose} className="px-3 py-1.5 text-xs rounded text-text-tertiary hover:text-text-primary">
            {tf('source.close')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full bg-surface-1">
      <div className="flex items-center justify-between px-3 py-1.5 border-b border-border-subtle bg-surface-2">
        <div className="flex items-center gap-2 text-xs text-text-tertiary">
          <Code2 size={14} />
          <span className="font-medium">{tf('source.title')}</span>
          {memberFile && (
            <span className="font-mono text-text-secondary" title={tf('source.memberFileTitle')}>
              · {memberFile}
            </span>
          )}
          {dirty && <span className="text-semantic-warning">{tf('source.unsaved')}</span>}
          {readOnly && <span className="text-2xs text-text-tertiary">{tf('source.readOnly')}</span>}
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setAddingAction(a => !a)}
            disabled={saving || !!document?.isFolder || readOnly}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
            title={document?.isFolder ? tf('source.addActionFolderTitle') : tf('source.addActionTitle')}
          >
            <Plus size={12} />
            {tf('source.addAction')}
          </button>
          <button
            onClick={handleRevert}
            disabled={!dirty || saving}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
          >
            <RotateCcw size={12} />
            {tf('source.revert')}
          </button>
          <button
            onClick={handleSave}
            disabled={!dirty || saving || readOnly || cloudFolder}
            title={cloudFolder ? tf('source.cloudFolderSaveTitle') : undefined}
            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
          >
            {saving ? <Spinner size={12} /> : <Save size={12} />}
            {saving ? tf('source.saving') : tf('source.save')}
          </button>
          <button
            onClick={handleClose}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 transition-colors"
          >
            {tf('source.blockView')}
          </button>
        </div>
      </div>

      {addingAction && <AddActionForm onInsert={handleInsertAction} onClose={() => setAddingAction(false)} />}

      <div className="flex flex-1 overflow-hidden font-mono text-xs">
        <LineGutter lineCount={lineCount} textareaRef={textareaRef} />
        <textarea
          ref={textareaRef}
          value={source}
          onChange={e => setSource(e.target.value)}
          spellCheck={false}
          className="flex-1 bg-surface-1 text-text-primary p-2 outline-hidden resize-none leading-5 whitespace-pre overflow-auto"
          style={{tabSize: 4}}
        />
      </div>
    </div>
  )
}
