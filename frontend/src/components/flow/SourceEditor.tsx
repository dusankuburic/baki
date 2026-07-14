import {useState, useEffect, useRef, useCallback} from 'react'
import {Save, RotateCcw, Code2} from 'lucide-react'
import {flowApi, analysisApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useToast, Spinner} from '@/components/shared'
import type {AnalysisReport} from '@/types'

interface SourceEditorProps {
  onClose: () => void
}

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

  const [source, setSource] = useState('')
  const [original, setOriginal] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const gutterRef = useRef<HTMLDivElement>(null)

  // Load the raw source on mount.
  useEffect(() => {
    if (!document) return
    let cancelled = false
    setLoading(true)
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
    if (!document || !dirty || saving) return
    setSaving(true)
    try {
      const updated = await flowApi.saveSource(document.id, source)
      setDocument(updated)
      setOriginal(source)
      // Best-effort re-analysis so findings reflect the edited source.
      try {
        const r = await analysisApi.analyzeFlow()
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
          {dirty && <span className="text-amber-500">• unsaved</span>}
        </div>
        <div className="flex items-center gap-2">
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
            disabled={!dirty || saving}
            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-surface-3 disabled:opacity-40 transition-colors"
          >
            {saving ? <Spinner size={12} /> : <Save size={12} />}
            {saving ? 'Saving…' : 'Save & Re-parse'}
          </button>
          <button onClick={onClose} className="text-2xs text-text-tertiary hover:text-text-secondary px-2 py-1 rounded hover:bg-surface-3 transition-colors">
            Block view
          </button>
        </div>
      </div>

      {/* Editor: line-number gutter + textarea */}
      <div className="flex flex-1 overflow-hidden font-mono text-xs">
        <div
          ref={gutterRef}
          className="flex-shrink-0 overflow-hidden py-2 pr-2 pl-3 text-right text-text-muted select-none bg-surface-2 border-r border-border-subtle"
          style={{minWidth: '3rem'}}
        >
          {Array.from({length: lineCount}, (_, i) => (
            <div key={i} className="leading-5">
              {i + 1}
            </div>
          ))}
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
