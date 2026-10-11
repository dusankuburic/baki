import {useState, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Search} from 'lucide-react'
import Input from '@/components/shared/Input'
import {shortcuts, formatShortcutParts} from '@/lib/shortcuts'

export default function ShortcutsPanel() {
  const {t} = useTranslation('settings')
  const [search, setSearch] = useState('')

  const grouped = useMemo(() => {
    const map = new Map<string, typeof shortcuts>()
    for (const s of shortcuts) {
      if (
        search &&
        !s.description.toLowerCase().includes(search.toLowerCase()) &&
        !s.id.toLowerCase().includes(search.toLowerCase())
      ) {
        continue
      }
      const list = map.get(s.category) || []
      list.push(s)
      map.set(s.category, list)
    }
    return map
  }, [search])

  return (
    <div>
      <Input
        icon={Search}
        value={search}
        onChange={e => setSearch((e.target as HTMLInputElement).value)}
        placeholder={t('shortcuts.searchPlaceholder')}
        aria-label={t('shortcuts.searchAria')}
        className="mb-4"
      />

      <div className="columns-[15rem] gap-x-8">
        {Array.from(grouped.entries()).map(([category, items]) => (
          <div key={category} className="mb-6">
            <h3 className="text-sm font-semibold text-text-secondary uppercase tracking-wider mb-2 break-after-avoid">
              {category}
            </h3>
            <div className="space-y-1">
              {items.map(s => (
                <div
                  key={s.id}
                  className="break-inside-avoid flex items-center justify-between py-1.5 px-2 rounded hover:bg-surface-2"
                >
                  <span className="text-sm text-text-primary">{s.description}</span>
                  <div className="flex items-center gap-1">
                    {formatShortcutParts(s.keys).map((part, pi) => (
                      <span key={pi} className="flex items-center gap-1">
                        {pi > 0 && <span className="text-text-tertiary text-xs">+</span>}
                        <kbd className="text-xs px-1.5 py-0.5 rounded bg-surface-3 border border-border-default text-text-secondary font-mono">
                          {part.display}
                        </kbd>
                      </span>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
