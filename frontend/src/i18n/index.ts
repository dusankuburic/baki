import i18next from 'i18next'
import {initReactI18next} from 'react-i18next'
import {en} from './en'
import {enSettings} from './en.settings'
import {enDashboard} from './en.dashboard'
import {enAdmin} from './en.admin'
import {enInspector} from './en.inspector'
import {enLibrary} from './en.library'
import {enFlow} from './en.flow'
import {enSearch} from './en.search'
import {enAnalyzer} from './en.analyzer'
import {enCollaboration} from './en.collaboration'

export const LANGUAGE_KEY = 'pad-language'
const SUPPORTED = ['en']

function detectLanguage(): string {
  try {
    const stored = localStorage.getItem(LANGUAGE_KEY)
    if (stored && SUPPORTED.includes(stored)) return stored
    const nav = navigator.language?.slice(0, 2)
    if (nav && SUPPORTED.includes(nav)) return nav
  } catch {
  }
  return 'en'
}

void i18next.use(initReactI18next).init({
  lng: detectLanguage(),
  fallbackLng: 'en',
  resources: {
    en: {
      common: en.common,
      shell: en.shell,
      findings: en.findings,
      auth: en.auth,
      chat: en.chat,
      settings: enSettings,
      dashboard: enDashboard,
      admin: enAdmin,
      inspector: enInspector,
      library: enLibrary,
      flow: enFlow,
      search: enSearch,
      analyzer: enAnalyzer,
      collaboration: enCollaboration,
    },
  },
  defaultNS: 'common',
  ns: ['common', 'shell', 'findings', 'auth', 'chat', 'settings', 'dashboard', 'admin', 'inspector', 'library', 'flow', 'search', 'analyzer', 'collaboration'],
  interpolation: {
    escapeValue: false,
  },
  react: {
    bindI18n: 'languageChanged',
  },
  returnEmptyString: false,
})

function syncHtmlLang(lng: string) {
  document.documentElement.lang = lng
}
syncHtmlLang(i18next.language)
i18next.on('languageChanged', syncHtmlLang)

export function setLanguage(lng: string) {
  try {
    localStorage.setItem(LANGUAGE_KEY, lng)
  } catch {
  }
  void i18next.changeLanguage(lng)
}

export default i18next
