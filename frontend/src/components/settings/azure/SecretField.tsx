import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Button, FormField, Input} from '@/components/shared'

interface SecretFieldProps {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  onBlur?: () => void
  /** "Leave blank to keep the stored secret" — only when the save would really keep it. */
  placeholder?: string
  hint?: string
  error?: string
}

// A secret (API key, client secret): a password field with a Show/Hide
// toggle. No autofill ("new-password" — password managers ignore "off" on a
// password field and would fill the user's saved site password), and no
// spellcheck, autocapitalise or autocorrect: once shown as plain text, a
// browser's spellcheck service could otherwise receive it. The toggle is a
// pressed-state button whose name says which secret it reveals.
export default function SecretField({id, label, value, onChange, onBlur, placeholder, hint, error}: SecretFieldProps) {
  const {t} = useTranslation('settings')
  const [shown, setShown] = useState(false)
  return (
    <FormField id={id} label={label}>
      <div className="flex items-start gap-2">
        <Input
          id={id}
          type={shown ? 'text' : 'password'}
          value={value}
          onChange={e => onChange(e.target.value)}
          onBlur={onBlur}
          placeholder={placeholder}
          hint={hint}
          error={error}
          autoComplete="new-password"
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          className="min-w-0 flex-1"
        />
        <Button
          size="sm"
          variant="secondary"
          className="mt-1"
          onClick={() => setShown(s => !s)}
          aria-pressed={shown}
          aria-label={
            shown
              ? t('providers.azureHideSecretAria', {field: label})
              : t('providers.azureShowSecretAria', {field: label})
          }
        >
          {shown ? t('apiKey.hide') : t('apiKey.show')}
        </Button>
      </div>
    </FormField>
  )
}
