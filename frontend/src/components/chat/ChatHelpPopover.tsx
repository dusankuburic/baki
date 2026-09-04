import {Trans, useTranslation} from 'react-i18next'
import {Kbd, Modal} from '@/components/shared'
import {SLASH_COMMANDS} from './SlashCommandAutocomplete'

interface Props {
  onClose: () => void
}

// `keys` are literal keycaps and stay untranslated; `descriptionKey` points at
// the localized explanation.
// `as const` keeps descriptionKey a literal union so the typed t() accepts it —
// a widened `string` would defeat the key checking the i18next augmentation adds.
const SHORTCUTS = [
  {keys: 'Enter', descriptionKey: 'help.sendMessage'},
  {keys: 'Shift/Cmd/Ctrl + Enter', descriptionKey: 'help.newLine'},
  {keys: '↑ / ↓', descriptionKey: 'help.cycleHistory'},
  {keys: '@', descriptionKey: 'help.mentionFile'},
  {keys: '/', descriptionKey: 'help.openCommandMenu'},
] as const

// ChatHelpPopover lists the slash commands and keyboard shortcuts. Backs the
// /help slash command.
//
// Built on the shared <Modal>: the hand-rolled version put role="dialog" and
// the accessible name on the BACKDROP rather than the panel, and had a
// window-level Escape listener with no focus trap, no initial focus and no
// focus restoration.
export default function ChatHelpPopover({onClose}: Props) {
  const {t} = useTranslation('chat')

  return (
    <Modal isOpen onClose={onClose} title={t('help.title')} size="sm">
      <div className="space-y-4">
        <div>
          <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">
            {t('help.commands')}
          </p>
          <ul className="space-y-1.5">
            {SLASH_COMMANDS.map(c => (
              <li key={c.id} className="flex items-baseline gap-2 text-sm">
                <code className="text-brand-400 font-medium">{c.id}</code>
                <span className="text-text-tertiary text-xs">{t(`commands.descriptions.${c.copyKey}`)}</span>
              </li>
            ))}
          </ul>
        </div>

        <div>
          <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">
            {t('help.shortcuts')}
          </p>
          <ul className="space-y-1.5">
            {SHORTCUTS.map(s => (
              <li key={s.keys} className="flex items-baseline justify-between gap-3 text-sm">
                <span className="text-text-secondary text-xs">{t(s.descriptionKey)}</span>
                <Kbd keys={[s.keys]} size="xs" />
              </li>
            ))}
          </ul>
        </div>

        <div>
          <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">
            {t('help.toolsHeading')}
          </p>
          <ul className="space-y-1.5 text-xs text-text-tertiary">
            <li>
              <span className="text-text-secondary font-medium">{t('help.toolsToggleName')}</span>{' '}
              {t('help.toolsToggleBody')}
            </li>
            <li>
              <span className="text-text-secondary font-medium">{t('help.fixApprovalName')}</span>{' '}
              <Trans t={t} i18nKey="help.fixApprovalBody" components={{em: <em />}} /> <Kbd keys={['Esc']} size="xs" />{' '}
              {t('help.escDismisses')}
            </li>
          </ul>
        </div>
      </div>
    </Modal>
  )
}
