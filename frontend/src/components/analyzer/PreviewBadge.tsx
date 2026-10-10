import {useTranslation} from 'react-i18next'
import {FlaskConical} from 'lucide-react'
import {Badge} from '@/components/shared'

// PreviewBadge marks a preview rule (catalog) or a preview rule's finding:
// reported like any other, but never a gate and not in the health score
// (core/analyzer Stage). Callers render it for stage === 'preview' only; a
// stage this bundle does not know reads as stable.
export default function PreviewBadge({of}: {of: 'rule' | 'finding'}) {
  const {t} = useTranslation('analyzer')
  const hint = of === 'rule' ? t('stage.previewRuleHint') : t('stage.previewFindingHint')
  return (
    <Badge tone="brand" icon={FlaskConical} title={hint} srLabel={hint}>
      {t('stage.preview')}
    </Badge>
  )
}
