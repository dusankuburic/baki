import {useState} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {X, Plus, GripVertical} from 'lucide-react'

const EMPTY_PROMPTS = {block: [], flow: [], finding: [], blockWithFindings: []}

export default function AIPromptsPanel() {
  const {settings, updateAI} = useSettingsStore()
  // Defensive: a prompts object (or any of its arrays) can be null/undefined if
  // settings were persisted before this feature existed; coalesce so the lists
  // below never map over a null and crash the panel.
  const p = settings.ai.prompts ?? EMPTY_PROMPTS

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Suggested Prompts</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Customize the quick-action prompts that appear above the chat input in different contexts.
      </p>

      <div className="space-y-8">
        <PromptList 
          title="Flow Context" 
          description="Shown when analyzing the entire flow without a specific block selected."
          items={p.flow}
          onChange={(newItems) => updateAI({prompts: {...p, flow: newItems}})}
        />
        
        <PromptList 
          title="Block Context" 
          description="Shown when a specific block is selected."
          items={p.block}
          onChange={(newItems) => updateAI({prompts: {...p, block: newItems}})}
        />

        <PromptList 
          title="Finding Context" 
          description="Shown when viewing a specific analysis finding."
          items={p.finding}
          onChange={(newItems) => updateAI({prompts: {...p, finding: newItems}})}
        />

        <PromptList 
          title="Block + Findings Context" 
          description="Shown when selecting a block that has active analysis findings."
          items={p.blockWithFindings}
          onChange={(newItems) => updateAI({prompts: {...p, blockWithFindings: newItems}})}
        />
      </div>
    </div>
  )
}

function PromptList({title, description, items, onChange}: {title: string, description: string, items: string[] | null | undefined, onChange: (items: string[]) => void}) {
  const [newItemText, setNewItemText] = useState('')
  // Tolerate a null/undefined list (legacy settings) so add/remove/edit and the
  // render below never spread or map over a non-array.
  const list = items ?? []

  const handleAdd = (e: React.FormEvent) => {
    e.preventDefault()
    if (!newItemText.trim()) return
    onChange([...list, newItemText.trim()])
    setNewItemText('')
  }

  const handleRemove = (index: number) => {
    const copy = [...list]
    copy.splice(index, 1)
    onChange(copy)
  }

  const handleEdit = (index: number, val: string) => {
    const copy = [...list]
    copy[index] = val
    onChange(copy)
  }

  return (
    <div className="bg-surface-2 rounded-lg border border-border-default overflow-hidden">
      <div className="px-4 py-3 border-b border-border-default bg-surface-1">
        <h3 className="text-sm font-medium text-text-primary">{title}</h3>
        <p className="text-xs text-text-tertiary mt-0.5">{description}</p>
      </div>
      
      <div className="p-2 space-y-1">
        {list.map((item, i) => (
          <div key={i} className="flex items-center gap-2 group p-1">
            <GripVertical className="w-4 h-4 text-text-tertiary cursor-grab active:cursor-grabbing opacity-0 group-hover:opacity-100" />
            <input
              type="text"
              className="flex-1 bg-transparent border border-transparent hover:border-border-default focus:border-brand-500 focus:bg-surface-1 rounded px-2 py-1 text-sm text-text-primary outline-none transition-colors"
              value={item}
              onChange={(e) => handleEdit(i, e.target.value)}
            />
            <button 
              onClick={() => handleRemove(i)}
              className="p-1.5 text-text-tertiary hover:text-red-400 hover:bg-red-400/10 rounded opacity-0 group-hover:opacity-100 transition-all duration-fast"
              title="Remove prompt"
            >
              <X className="w-4 h-4" />
            </button>
          </div>
        ))}

        <form onSubmit={handleAdd} className="flex items-center gap-2 p-1 pl-7 mt-2">
          <input
            type="text"
            className="flex-1 bg-surface-3 border border-border-default rounded px-2 py-1.5 text-sm text-text-primary outline-none focus:border-brand-500 placeholder:text-text-tertiary"
            placeholder="Add a new suggested prompt..."
            value={newItemText}
            onChange={(e) => setNewItemText(e.target.value)}
          />
          <button 
            type="submit"
            disabled={!newItemText.trim()}
            className="p-1.5 text-brand-500 hover:bg-brand-500/10 rounded disabled:opacity-50 disabled:hover:bg-transparent transition-colors"
          >
            <Plus className="w-5 h-5" />
          </button>
        </form>
      </div>
    </div>
  )
}