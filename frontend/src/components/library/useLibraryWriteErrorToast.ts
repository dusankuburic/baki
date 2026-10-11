import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {VersionConflictError} from '@/api/client'
import {useToast} from '@/components/shared'

export function useLibraryWriteErrorToast() {
  const {t} = useTranslation('shell')
  const toast = useToast()
  return useCallback(
    (err: unknown, failedTitle: string, onReload: () => void) => {
      if (err instanceof VersionConflictError) {
        toast.warning(t('libraryTab.conflictTitle'), {
          description: t('libraryTab.conflictBody'),
          duration: 15000,
          action: {label: t('libraryTab.reload'), onClick: onReload},
        })
        return
      }
      toast.error(failedTitle, {description: getErrorMessage(err, t('libraryTab.unknownError'))})
    },
    [t, toast],
  )
}
