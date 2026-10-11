import {useTranslation} from 'react-i18next'
import {FlaskConical} from 'lucide-react'
import {Badge} from '@/components/shared'

export default function PreviewBadge({of}: {of: 'rule' | 'finding'}) {
  const {t} = useTranslation('analyzer')
  const hint = of === 'rule' ? t('stage.previewRuleHint') : t('stage.previewFindingHint')
  return (
    <Badge tone="brand" icon={FlaskConical} title={hint} srLabel={hint}>
      {t('stage.preview')}
    </Badge>
  )
}
