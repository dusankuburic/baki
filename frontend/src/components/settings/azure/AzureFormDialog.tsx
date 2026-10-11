import {useCallback, useRef, type ReactNode, type RefObject} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Button, Modal, useConfirm} from '@/components/shared'

interface AzureFormDialogProps {
  title: string
  description?: string
  size: 'md' | 'lg'
  formId: string
  dirty: boolean
  busy: boolean
  submitLabel: string
  onSubmit: () => void
  onClose: () => void
  formAlert?: ReactNode
  footerStart?: ReactNode
  initialFocusRef?: RefObject<HTMLElement | null>
  children: ReactNode
}

export default function AzureFormDialog({
  title,
  description,
  size,
  formId,
  dirty,
  busy,
  submitLabel,
  onSubmit,
  onClose,
  formAlert,
  footerStart,
  initialFocusRef,
  children,
}: AzureFormDialogProps) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  const confirming = useRef(false)

  const requestClose = useCallback(async () => {
    if (busy || confirming.current) return
    if (dirty) {
      confirming.current = true
      let discard: boolean
      try {
        discard = await confirm({
          title: t('azureModels.discard.title'),
          message: t('azureModels.discard.message'),
          danger: true,
          confirmLabel: t('azureModels.discard.confirm'),
        })
      } finally {
        confirming.current = false
      }
      if (!discard) return
    }
    onClose()
  }, [busy, confirm, dirty, onClose, t])

  return (
    <Modal
      isOpen
      onClose={() => void requestClose()}
      title={title}
      description={description}
      size={size}
      fullScreenBelow="sm"
      closeOnBackdrop={false}
      initialFocusRef={initialFocusRef}
      footer={
        <div className="flex w-full flex-wrap items-start gap-3">
          {footerStart && <div className="min-w-0 flex-1 basis-56">{footerStart}</div>}
          <div className="ml-auto flex shrink-0 items-center gap-2">
            <Button size="sm" variant="secondary" onClick={() => void requestClose()} disabled={busy}>
              {t('cancel', {ns: 'common'})}
            </Button>
            <Button size="sm" variant="primary" type="submit" form={formId} loading={busy}>
              {submitLabel}
            </Button>
          </div>
        </div>
      }
    >
      <div
        role="alert"
        className={clsx(
          formAlert &&
            'mb-4 rounded-lg border border-semantic-error/30 bg-semantic-error/10 px-3 py-2 text-sm text-text-primary',
        )}
      >
        {formAlert}
      </div>
      <form
        id={formId}
        noValidate
        className="space-y-4"
        onSubmit={e => {
          e.preventDefault()
          if (!busy) onSubmit()
        }}
      >
        {children}
      </form>
    </Modal>
  )
}
