import {useState, useMemo} from 'react'
import Input from '@/components/shared/Input'
import {shortcuts, formatShortcutParts} from '@/lib/shortcuts'

export default function ShortcutsPanel() {
  const [search, setSearch] = useState('')

  const grouped = useMemo(() => {
    const map = new Map<string, typeof shortcuts>()
    for (const s of shortcuts) {
      if (search && !s.description.toLowerCase().includes(search.toLowerCase()) && !s.id.toLowerCase().includes(search.toLowerCase())) {
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
      <h2 className="text-xl font-semibold text-text-primary">Keyboard Shortcuts</h2>
      <p className="text-sm text-text-secondary mt-1 mb-4">
        Reference of all available keyboard shortcuts.
      </p>

      <Input
        value={search}
        onChange={(e) => setSearch((e.target as HTMLInputElement).value)}
        placeholder="Search shortcuts..."
        className="mb-4"
      />

      <div className="space-y-6">
        {Array.from(grouped.entries()).map(([category, items]) => (
          <div key={category}>
            <h3 className="text-sm font-semibold text-text-secondary uppercase tracking-wider mb-2">{category}</h3>
            <div className="space-y-1">
              {items.map(s => (
                <div key={s.id} className="flex items-center justify-between py-1.5 px-2 rounded hover:bg-surface-2">
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
