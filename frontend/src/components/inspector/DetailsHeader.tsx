import {getBlockIcon, getBlockColor, getBlockBg} from '@/lib/blocks'
import type {Block} from '@/types'
import type {LucideIcon} from 'lucide-react'

type DetailsHeaderProps = {
  block: Block
}

export default function DetailsHeader({block}: DetailsHeaderProps) {
  const Icon = getBlockIcon(block.type) as LucideIcon
  const color = getBlockColor(block.type)
  const bg = getBlockBg(block.type)

  return (
    <div className="flex items-center gap-3 px-4 py-4 border-b border-border-subtle">
      <div
        className="w-12 h-12 rounded-lg flex items-center justify-center flex-shrink-0"
        style={{backgroundColor: bg}}
      >
        <Icon size={24} style={{color}} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="text-xs font-medium uppercase tracking-wider" style={{color}}>
          {block.type}
        </div>
        <div className="text-base font-semibold text-text-primary truncate">{block.name}</div>
      </div>
    </div>
  )
}
