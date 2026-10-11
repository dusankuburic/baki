import clsx from 'clsx'
import {Check, Circle} from 'lucide-react'
import {useTranslation} from 'react-i18next'
import {PASSWORD_CLASSES, PASSWORD_MIN_LENGTH, type PasswordCheck} from '@/lib/password'

export function PasswordRuleList({id, check}: {id: string; check: PasswordCheck}) {
  const {t} = useTranslation('auth')
  return (
    <ul id={id} aria-label={t('password.rulesLabel')} className="mt-2 space-y-1">
      <Rule met={check.longEnough}>{t('password.ruleLength', {count: PASSWORD_MIN_LENGTH})}</Rule>
      <Rule met={check.enoughClasses}>
        {t('password.ruleClasses')}
        <span className="inline-flex flex-wrap gap-1 ml-1.5 align-middle">
          {PASSWORD_CLASSES.map(c => (
            <span
              key={c}
              className={clsx(
                'px-1.5 rounded text-2xs font-medium leading-4',
                check.classes[c] ? 'bg-semantic-success/10 text-semantic-success' : 'bg-surface-3 text-text-tertiary',
              )}
            >
              {t(`passwordClass.${c}`)}
            </span>
          ))}
        </span>
      </Rule>
    </ul>
  )
}

function Rule({met, children}: {met: boolean; children: React.ReactNode}) {
  const {t} = useTranslation('auth')
  const Icon = met ? Check : Circle
  return (
    <li
      className={clsx('flex items-start gap-1.5 text-xs', met ? 'text-text-secondary' : 'text-text-tertiary')}
      data-met={met || undefined}
    >
      <Icon
        size={13}
        className={clsx('mt-px shrink-0', met ? 'text-semantic-success' : 'text-text-disabled')}
        aria-hidden="true"
      />
      <span className="sr-only">{met ? t('password.ruleMet') : t('password.ruleUnmet')}</span>
      <span>{children}</span>
    </li>
  )
}
