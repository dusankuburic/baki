import {useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import {useOptionalToast} from '@/components/shared'
import {useSettingsStore} from '@/stores/settingsStore'

/** Shows a toast for every refused settings save (the store has already put the document back and re-read it), in
 * the server's words: a 400 names the field it refused. Mounted once, inside the toast provider. */
export function useSettingsSaveToast(): void {
  const {t} = useTranslation('settings')
  const toast = useOptionalToast()
  useEffect(() => {
    if (!toast) return
    return useSettingsStore.subscribe((state, prev) => {
      const error = state.saveError
      if (!error || error === prev.saveError) return
      toast.error(t('saveFailed.title'), {description: error.message || t('saveFailed.reloaded')})
    })
  }, [toast, t])
}
