import {useTranslation} from 'react-i18next'
import {CollapsibleSection} from './CollapsibleSection'
import type {Block} from '@/types'

type BlockMetadataProps = {
  block: Block
  subflowName?: string
}

export default function BlockMetadata({block, subflowName}: BlockMetadataProps) {
  const {t} = useTranslation('inspector')
  return (
    <CollapsibleSection title={t('details.location')}>
      <div className="space-y-2">
        {subflowName && (
          <div className="flex gap-2">
            <span className="text-xs text-text-tertiary font-medium w-2/5">{t('details.subflow')}</span>
            <span className="text-sm text-text-primary">{subflowName}</span>
          </div>
        )}
        <div className="flex gap-2">
          <span className="text-xs text-text-tertiary font-medium w-2/5">{t('details.line')}</span>
          <span className="text-sm text-text-primary">{block.lineNumber}</span>
        </div>
        <div className="flex gap-2">
          <span className="text-xs text-text-tertiary font-medium w-2/5">{t('details.depth')}</span>
          <span className="text-sm text-text-primary">{t('details.depthLevels', {count: block.indent})}</span>
        </div>
      </div>
    </CollapsibleSection>
  )
}
