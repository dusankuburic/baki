import {useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Plus, X} from 'lucide-react'
import {Button} from '@/components/shared'

export function composeActionLine(rawType: string, params: string, output: string): string {
  let line = rawType.trim()
  const p = params.trim()
  if (p) line += ' ' + p
  const o = output.trim()
  if (o) line += ' => ' + o
  return line
}

export function insertBeforeLastRegionEnd(source: string, line: string): string {
  const lines = source.split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i]?.trim().toLowerCase() === '#endregion') {
      const out = [...lines]
      out.splice(i, 0, '    ' + line)
      return out.join('\n')
    }
  }
  const sep = source.endsWith('\n') ? '' : '\n'
  return source + sep + '    ' + line + '\n'
}

interface Props {
  onInsert: (line: string) => void
  onClose: () => void
}

export default function AddActionForm({onInsert, onClose}: Props) {
  const {t} = useTranslation('flow')
  const [rawType, setRawType] = useState('')
  const [params, setParams] = useState('')
  const [output, setOutput] = useState('')

  const preview = useMemo(() => composeActionLine(rawType, params, output), [rawType, params, output])
  const valid = rawType.trim().length > 0 && /^[A-Za-z][A-Za-z0-9_.]*$/.test(rawType.trim())

  return (
    <div
      className="mx-3 my-2 rounded-lg border border-brand-500/30 bg-brand-500/5 p-3"
      data-testid="add-action-form"
      role="dialog"
      aria-label={t('addAction.ariaLabel')}
    >
      <div className="flex items-center justify-between mb-2">
        <span className="text-2xs font-semibold uppercase tracking-wide text-brand-300">{t('addAction.heading')}</span>
        <button
          onClick={onClose}
          className="p-1 rounded text-text-tertiary hover:text-text-secondary"
          aria-label={t('addAction.closeAria')}
        >
          <X size={12} />
        </button>
      </div>
      <div className="space-y-2">
        <input
          value={rawType}
          onChange={e => setRawType(e.target.value)}
          placeholder={t('addAction.typePlaceholder')}
          autoFocus
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-hidden focus:border-brand-500 font-mono"
          aria-label={t('addAction.typeAria')}
        />
        <input
          value={params}
          onChange={e => setParams(e.target.value)}
          placeholder={t('addAction.paramsPlaceholder')}
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-hidden focus:border-brand-500 font-mono"
          aria-label={t('addAction.paramsAria')}
        />
        <input
          value={output}
          onChange={e => setOutput(e.target.value)}
          placeholder={t('addAction.outputPlaceholder')}
          className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-hidden focus:border-brand-500 font-mono"
          aria-label={t('addAction.outputAria')}
        />
        {preview && (
          <div className="rounded bg-surface-2 border border-border-subtle px-2 py-1.5">
            <div className="text-2xs text-text-tertiary mb-0.5">{t('addAction.preview')}</div>
            <code className="text-2xs text-brand-300 font-mono break-all">{preview}</code>
          </div>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t('addAction.cancel')}
          </Button>
          <Button
            variant="primary"
            size="sm"
            disabled={!valid}
            icon={Plus}
            onClick={() => {
              onInsert(preview)
            }}
          >
            {t('addAction.insert')}
          </Button>
        </div>
        {!valid && rawType.trim().length > 0 && (
          <p className="text-2xs text-semantic-warning">{t('addAction.invalidType')}</p>
        )}
      </div>
    </div>
  )
}
