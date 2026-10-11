import {useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {useConfirm} from '@/components/shared'

export function useSuppressReasonPrompt() {
  const {t} = useTranslation('findings')
  const {prompt} = useConfirm()
  return useCallback(
    (count = 1) =>
      prompt({
        title: t('suppressPrompt.title', {count}),
        label: t('suppressPrompt.label'),
        placeholder: t('suppressPrompt.placeholder'),
        confirmLabel: t('suppressPrompt.confirm'),
      }),
    [prompt, t],
  )
}
