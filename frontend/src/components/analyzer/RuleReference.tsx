import React, {useState, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Search, Wrench, AlertTriangle, Info, BookOpen} from 'lucide-react'
import {analysisApi} from '@/api/analysis'
import {useOrgStore} from '@/stores/orgStore'
import type {Rule} from '@/types/analysis'
import {Spinner} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import PreviewBadge from './PreviewBadge'

const SEVERITY_ICONS: Record<string, React.ReactNode> = {
  error: <AlertTriangle className="w-4 h-4 text-red-400" />,
  warning: <AlertTriangle className="w-4 h-4 text-yellow-400" />,
  info: <Info className="w-4 h-4 text-blue-400" />,
}

export default function RuleReference() {
  const {t} = useTranslation('analyzer')
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<string>('all')

  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const {
    data: rules,
    isLoading,
    error,
  } = useAsync(async () => {
    return await analysisApi.getRules(activeOrgId ?? undefined)
  }, [activeOrgId])

  const categories = useMemo(() => {
    if (!rules) return []
    const cats = new Set(rules.map((r: Rule) => r.category))
    return ['all', ...Array.from(cats).sort()]
  }, [rules])

  const filtered = useMemo(() => {
    if (!rules) return []
    return rules
      .filter((r: Rule) => filter === 'all' || r.category === filter)
      .filter((r: Rule) => {
        if (!query.trim()) return true
        const q = query.toLowerCase()
        return (
          r.id.toLowerCase().includes(q) || r.name.toLowerCase().includes(q) || r.description.toLowerCase().includes(q)
        )
      })
      .sort((a: Rule, b: Rule) => a.category.localeCompare(b.category) || a.id.localeCompare(b.id))
  }, [rules, filter, query])

  const autoFixableCount = rules?.filter((r: Rule) => r.autoFix).length ?? 0
  const previewCount = rules?.filter((r: Rule) => r.stage === 'preview').length ?? 0

  if (isLoading) return <Spinner />
  if (error) return <div className="p-8 text-center text-text-secondary">{t('reference.loadFailed')}</div>

  return (
    <div className="h-full overflow-y-auto p-6 max-w-5xl mx-auto">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-text-primary flex items-center gap-2">
          <BookOpen className="w-6 h-6" />
          {t('reference.title')}
        </h1>
        <p className="text-sm text-text-secondary mt-1">
          {t('reference.summary', {
            rules: rules?.length ?? 0,
            categories: categories.length - 1,
            autoFixable: autoFixableCount,
          })}
          {previewCount > 0 && <> · {t('reference.previewCount', {count: previewCount})}</>}
        </p>
      </div>

      <div className="flex gap-3 mb-6">
        <div className="flex-1 relative">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-text-tertiary" />
          <input
            className="w-full pl-10 pr-4 py-2 rounded-lg bg-surface-2 border border-border-default text-sm text-text-primary placeholder:text-text-tertiary focus:outline-hidden focus:border-accent-blue"
            placeholder={t('reference.searchPlaceholder')}
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
        </div>
        <select
          className="px-3 py-2 rounded-lg bg-surface-2 border border-border-default text-sm text-text-primary focus:outline-hidden focus:border-accent-blue"
          value={filter}
          onChange={e => setFilter(e.target.value)}
        >
          {categories.map(c => (
            <option key={c} value={c}>
              {c === 'all' ? t('reference.allCategories') : c}
            </option>
          ))}
        </select>
      </div>

      <div className="space-y-3">
        {filtered.map((r: Rule) => (
          <div
            key={r.id}
            className="p-4 rounded-lg bg-surface-2 border border-border-default hover:border-border-hover transition-colors"
          >
            <div className="flex items-start justify-between gap-3">
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2 mb-1">
                  <code className="text-xs font-mono text-accent-blue">{r.id}</code>
                  {SEVERITY_ICONS[r.defaultSeverity]}
                  {r.stage === 'preview' && <PreviewBadge of="rule" />}
                  {r.autoFix && (
                    <span className="inline-flex items-center gap-1 text-xs text-green-400">
                      <Wrench className="w-3 h-3" />
                      {t('reference.autofix')}
                    </span>
                  )}
                </div>
                <h3 className="text-sm font-medium text-text-primary">{r.name}</h3>
                <p className="text-xs text-text-secondary mt-1">{r.description}</p>
                <div className="flex gap-3 mt-2 text-xs text-text-tertiary">
                  <span>{t('reference.category', {category: r.category})}</span>
                  <span>{t('reference.severity', {severity: r.defaultSeverity})}</span>
                  {r.confidence && <span>{t('reference.confidence', {confidence: r.confidence})}</span>}
                  {r.kind === 'library' && (
                    <span title={t('reference.libraryKindHint')}>{t('reference.libraryKind')}</span>
                  )}
                </div>
              </div>
            </div>
          </div>
        ))}
        {filtered.length === 0 && (
          <div className="text-center py-12 text-text-secondary">{t('reference.noMatches', {query})}</div>
        )}
      </div>
    </div>
  )
}
