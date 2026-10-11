import type {common, shell, findings, auth, chat} from './en'
import type {enSettings} from './en.settings'
import type {enDashboard} from './en.dashboard'
import type {enAdmin} from './en.admin'
import type {enInspector} from './en.inspector'
import type {enLibrary} from './en.library'
import type {enFlow} from './en.flow'
import type {enSearch} from './en.search'
import type {enAnalyzer} from './en.analyzer'
import type {enCollaboration} from './en.collaboration'

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
      dashboard: typeof enDashboard
      admin: typeof enAdmin
      inspector: typeof enInspector
      library: typeof enLibrary
      flow: typeof enFlow
      search: typeof enSearch
      analyzer: typeof enAnalyzer
      collaboration: typeof enCollaboration
    }
  }
}
