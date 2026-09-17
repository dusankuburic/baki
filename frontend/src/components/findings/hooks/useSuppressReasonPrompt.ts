import {useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {useConfirm} from '@/components/shared'

// useSuppressReasonPrompt asks the user why they are suppressing (one finding
// or a batch) and resolves the trimmed reason, or null when they cancel —
// callers must then leave the findings untouched. Suppressions used to store
// hard-coded strings ("Dismissed by user", "Bulk suppressed via selection"),
// so the team-shared triage record never said why a finding was waved off.
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
