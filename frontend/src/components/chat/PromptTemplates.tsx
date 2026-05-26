import {useState} from 'react'
import {BookOpen, ChevronDown, ChevronRight} from 'lucide-react'

export interface PromptTemplate {
  id: string
  label: string
  prompt: string
  category: 'analysis' | 'debug' | 'refactor' | 'explain'
}

const builtInTemplates: PromptTemplate[] = [
  {id: 't-explain-block', label: 'Explain block in detail', prompt: 'Explain what this block does step by step, including all properties and variables it uses.', category: 'explain'},
  {id: 't-explain-flow', label: 'Explain entire flow', prompt: 'Explain what this entire flow does from start to finish. Describe the main purpose, key decision points, and the overall logic.', category: 'explain'},
  {id: 't-find-bugs', label: 'Find bugs', prompt: 'Analyze this for potential bugs, race conditions, unhandled errors, and edge cases. List each issue with severity and a suggested fix.', category: 'debug'},
  {id: 't-error-handling', label: 'Check error handling', prompt: 'Review the error handling in this flow. Are there actions that could fail without proper error handlers? List missing error handling and suggest improvements.', category: 'debug'},
  {id: 't-performance', label: 'Performance review', prompt: 'Analyze this for performance issues. Look for unnecessary loops, redundant API calls, missing parallelism, and suggest optimizations.', category: 'analysis'},
  {id: 't-security', label: 'Security audit', prompt: 'Perform a security audit. Check for hardcoded credentials, injection vulnerabilities, insecure data handling, and missing input validation.', category: 'analysis'},
  {id: 't-simplify', label: 'Simplify logic', prompt: 'Suggest ways to simplify this flow. Look for redundant blocks, overly complex conditions, and opportunities to reduce nesting.', category: 'refactor'},
  {id: 't-variables', label: 'Variable usage review', prompt: 'Review variable usage in this flow. Find unused variables, variables that could be consolidated, and naming improvements.', category: 'refactor'},
]

interface Props {
  onSelect: (prompt: string) => void
  hasBlock: boolean
  flowName?: string
  blockName?: string
}

export default function PromptTemplates({onSelect, hasBlock: _hasBlock, flowName, blockName}: Props) {
  const [expanded, setExpanded] = useState(false)

  const interpolate = (prompt: string) => {
    let result = prompt
    if (flowName) result = result.replace(/\{flowName\}/g, flowName)
    if (blockName) result = result.replace(/\{blockName\}/g, blockName)
    return result
  }

  const categoryLabels: Record<string, string> = {
    explain: 'Explain',
    debug: 'Debug',
    analysis: 'Analysis',
    refactor: 'Refactor',
  }

  const grouped = builtInTemplates.reduce((acc, t) => {
    if (!acc[t.category]) acc[t.category] = []
    acc[t.category].push(t)
    return acc
  }, {} as Record<string, PromptTemplate[]>)

  return (
    <div className="px-3">
      <button
        className="flex items-center gap-1.5 text-xs text-text-secondary hover:text-text-primary transition-colors"
        onClick={() => setExpanded(v => !v)}
      >
        <BookOpen size={13} />
        <span>Templates</span>
        {expanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
      </button>

      {expanded && (
        <div className="mt-2 space-y-2">
          {Object.entries(grouped).map(([cat, templates]) => (
            <div key={cat}>
              <span className="text-2xs font-medium text-text-tertiary uppercase tracking-wider">
                {categoryLabels[cat] || cat}
              </span>
              <div className="flex flex-wrap gap-1.5 mt-1">
                {templates.map(t => (
                  <button
                    key={t.id}
                    className="bg-surface-2 hover:bg-surface-3 text-xs px-2.5 py-1 rounded-md whitespace-nowrap transition-colors border border-border-default"
                    onClick={() => onSelect(interpolate(t.prompt))}
                    title={t.prompt}
                  >
                    {t.label}
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
