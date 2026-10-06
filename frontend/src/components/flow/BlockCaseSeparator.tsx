import {ChevronDown, ChevronRight} from 'lucide-react'
import {useTranslation} from 'react-i18next'
import {getBlockColor} from '@/lib/blocks'
import {useFlowStore} from '@/stores/flowStore'
import type {Block} from '@/types'

type BlockCaseSeparatorProps = {
  block: Block
  collapsed: boolean
}

export default function BlockCaseSeparator({block, collapsed}: BlockCaseSeparatorProps) {
  const {t} = useTranslation('flow')
  const color = getBlockColor('SWITCH')
  const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
  const Chevron = collapsed ? ChevronRight : ChevronDown

  const isDefault = block.type === 'DEFAULT'
  const rawLabel = isDefault ? '' : block.name.replace(/^CASE\s+/i, '')
  const label = rawLabel
    .replace(/^\s*=\s*/, '')
    .replace(/^\$'''/, '')
    .replace(/'''$/, '')

  return (
    <div
      className="relative flex items-center gap-2 max-w-[450px] w-full rounded-lg select-none overflow-hidden"
      style={{
        backgroundColor: 'var(--block-condition-bg)',
        padding: '8px 10px 8px 14px',
      }}
    >
      <div
        className="absolute left-0 top-0 bottom-0 rounded-l-lg"
        style={{
          width: 4,
          backgroundColor: color,
        }}
      />
      <button
        className="flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 transition-colors shrink-0"
        onClick={e => {
          e.stopPropagation()
          toggleBlockExpand(block.id)
        }}
        aria-label={collapsed ? t('branches.expand') : t('branches.collapse')}
        aria-expanded={!collapsed}
      >
        <Chevron size={12} style={{color}} />
      </button>
      <span className="text-2xs font-semibold uppercase tracking-widest shrink-0" style={{color}}>
        {isDefault ? t('branches.default') : t('branches.case')}
      </span>
      {!isDefault && label && <span className="text-2xs text-text-secondary truncate">{label}</span>}
      {collapsed && block.children.length > 0 && (
        <span className="text-2xs text-text-tertiary ml-auto shrink-0">
          {t('branches.itemsCount', {count: block.children.length})}
        </span>
      )}
    </div>
  )
}
