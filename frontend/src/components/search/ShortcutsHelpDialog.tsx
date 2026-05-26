import {useState} from 'react'
import Modal from '@/components/shared/Modal'
import Input from '@/components/shared/Input'
import {shortcuts, formatShortcutParts} from '@/lib/shortcuts'

interface Props {
  isOpen: boolean
  onClose: () => void
}

export default function ShortcutsHelpDialog({isOpen, onClose}: Props) {
  const [search, setSearch] = useState('')

  const filtered = search
    ? shortcuts.filter(s =>
        s.description.toLowerCase().includes(search.toLowerCase()) ||
        s.keys.toLowerCase().includes(search.toLowerCase()))
    : shortcuts

  const grouped = new Map<string, typeof shortcuts>()
  for (const s of filtered) {
    const list = grouped.get(s.category) || []
    list.push(s)
    grouped.set(s.category, list)
  }

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Keyboard Shortcuts" size="lg">
      <Input
        value={search}
        onChange={(e) => setSearch((e.target as HTMLInputElement).value)}
        placeholder="Search shortcuts..."
        className="mb-4"
      />
      <div className="space-y-4 max-h-[50vh] overflow-y-auto">
        {Array.from(grouped.entries()).map(([category, items]) => (
          <div key={category}>
            <h3 className="text-xs font-semibold text-text-secondary uppercase tracking-wider mb-2">{category}</h3>
            <div className="space-y-1">
              {items.map(s => (
                <div key={s.id} className="flex items-center justify-between py-1 px-2 rounded hover:bg-surface-2">
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
    </Modal>
  )
}
