import {useTranslation} from 'react-i18next'
import {useId, useState} from 'react'
import clsx from 'clsx'
import {BookOpen, ChevronDown, ChevronRight} from 'lucide-react'

export type TemplateId =
  | 't-explain-block'
  | 't-explain-flow'
  | 't-find-bugs'
  | 't-error-handling'
  | 't-performance'
  | 't-security'
  | 't-simplify'
  | 't-variables'

export interface PromptTemplate {
  id: TemplateId
  label: string
  prompt: string
  category: 'analysis' | 'debug' | 'refactor' | 'explain'
}

// Ids + category only. Labels and prompts resolve through
// chat:templates.labels.<id> / chat:templates.prompts.<id>, so adding a locale
// does not mean forking this table.
// requiresBlock: the template talks about "this block", so it is only offered
// while a block is in scope — sent without one, the model had nothing to explain.
const builtInTemplates: {id: TemplateId; category: PromptTemplate['category']; requiresBlock?: boolean}[] = [
  {id: 't-explain-block', category: 'explain', requiresBlock: true},
  {id: 't-explain-flow', category: 'explain'},
  {id: 't-find-bugs', category: 'debug'},
  {id: 't-error-handling', category: 'debug'},
  {id: 't-performance', category: 'analysis'},
  {id: 't-security', category: 'analysis'},
  {id: 't-simplify', category: 'refactor'},
  {id: 't-variables', category: 'refactor'},
]

interface Props {
  onSelect: (prompt: string) => void
  hasBlock: boolean
  flowName?: string
  blockName?: string
  /**
   * Why nothing can be sent right now (the composer's guidance). The
   * templates stay reachable, marked unavailable, with this as their
   * description.
   */
  disabledReason?: string
}

export default function PromptTemplates({onSelect, hasBlock, flowName, blockName, disabledReason}: Props) {
  const {t} = useTranslation('chat')
  const [expanded, setExpanded] = useState(false)
  const reasonId = useId()

  const interpolate = (prompt: string) => {
    let result = prompt
    if (flowName) result = result.replace(/\{flowName\}/g, flowName)
    if (blockName) result = result.replace(/\{blockName\}/g, blockName)
    return result
  }

  const grouped = builtInTemplates
    .filter(tpl => hasBlock || !tpl.requiresBlock)
    .reduce(
      (acc, tpl) => {
        const resolved: PromptTemplate = {
          id: tpl.id,
          category: tpl.category,
          label: t(`templates.labels.${tpl.id}`),
          prompt: t(`templates.prompts.${tpl.id}`),
        }
        if (!acc[tpl.category]) acc[tpl.category] = []
        acc[tpl.category].push(resolved)
        return acc
      },
      {} as Record<string, PromptTemplate[]>,
    )

  return (
    <div className="chat-gutter">
      <button
        className="flex items-center gap-1.5 text-xs text-text-secondary hover:text-text-primary transition-colors"
        onClick={() => setExpanded(v => !v)}
      >
        <BookOpen size={13} />
        <span>{t('templates.ui.heading')}</span>
        {expanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
      </button>

      {expanded && (
        <div className="mt-2 space-y-2">
          {disabledReason && (
            <span id={reasonId} className="sr-only">
              {disabledReason}
            </span>
          )}
          {Object.entries(grouped).map(([cat, templates]) => (
            <div key={cat}>
              <span className="text-2xs font-medium text-text-tertiary uppercase tracking-wider">
                {t(`templates.categories.${cat}`, {defaultValue: cat})}
              </span>
              <div className="flex flex-wrap gap-1.5 mt-1">
                {templates.map(tpl => (
                  <button
                    key={tpl.id}
                    type="button"
                    aria-disabled={disabledReason ? true : undefined}
                    aria-describedby={disabledReason ? reasonId : undefined}
                    className={clsx(
                      'bg-surface-2 text-xs px-2.5 py-1 rounded-md whitespace-nowrap transition-colors border border-border-default',
                      disabledReason ? 'opacity-50 cursor-not-allowed' : 'hover:bg-surface-3',
                    )}
                    onClick={() => {
                      if (!disabledReason) onSelect(interpolate(tpl.prompt))
                    }}
                    title={disabledReason ?? tpl.prompt}
                  >
                    {tpl.label}
                  </button>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
