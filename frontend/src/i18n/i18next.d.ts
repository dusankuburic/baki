import type {common, shell, findings, auth, chat} from './en'
import type {enSettings} from './en.settings'

// Module augmentation: `t('findings.search.placeholder')` and friends are
// type-checked against the English source of truth — a typo'd key is a
// compile error, not a runtime blank.
declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'common'
    resources: {
      common: typeof common
      shell: typeof shell
      findings: typeof findings
      auth: typeof auth
      chat: typeof chat
      settings: typeof enSettings
    }
  }
}
