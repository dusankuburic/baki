import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback} from 'react'
import {useTranslation} from 'react-i18next'
import {VersionConflictError} from '@/api/client'
import {useToast} from '@/components/shared'

// useLibraryWriteErrorToast reports a failed library write. A 409 means the
// flow moved on under the caller (another user saved, or this view holds a
// stale version), so it gets the conflict toast with a Reload action instead
// of the raw "modified concurrently" message: retrying with the same version
// can only fail again. Everything else is an ordinary error toast.
//
// Shared by every library PUT site (sidebar save, rename, tags) so the
// recovery path cannot differ between them again.
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
