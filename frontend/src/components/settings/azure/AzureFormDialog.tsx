import {useCallback, useRef, type ReactNode, type RefObject} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Button, Modal, useConfirm} from '@/components/shared'

interface AzureFormDialogProps {
  title: string
  description?: string
  /** md = 512 px (the Model modal), lg = 672 px (the Resource modal). Full screen below 640 px either way. */
  size: 'md' | 'lg'
  /** The form's id: the footer sits outside the form, so its submit button names the form (form=). */
  formId: string
  /** Unsaved edits: Escape, the X and Cancel ask before they are discarded. */
  dirty: boolean
  /** A save is in flight: the dialog stays open until it settles. */
  busy: boolean
  submitLabel: string
  onSubmit: () => void
  /** Closes the dialog (after the discard confirm, when there was something to discard). */
  onClose: () => void
  /** The form-level alert: a server error no field owns, a conflict with its Reload. */
  formAlert?: ReactNode
  /** The footer's start: a Test button with its cost hint and outcome. */
  footerStart?: ReactNode
  /** The field that takes focus on open (the first one; the secret when entering credentials again). */
  initialFocusRef?: RefObject<HTMLElement | null>
  children: ReactNode
}

// The frame of the Resource and Model modals. The form validates on submit
// (noValidate: no browser bubbles), Save stays enabled, and a click on the
// backdrop does nothing — a stray click must not throw away a half-typed
// form. Every other way out (Escape, the X, Cancel) goes through
// requestClose, which asks before discarding unsaved edits; the confirm
// stacks on this dialog, so its own Escape closes only the confirm.
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
      {/* Always mounted: an error inserted into an existing alert region is
          what gets it announced. */}
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
