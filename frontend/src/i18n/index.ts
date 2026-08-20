import i18next from 'i18next'
import {initReactI18next} from 'react-i18next'
import {en} from './en'
import {enSettings} from './en.settings'

// Language persistence: the chosen locale survives reloads via localStorage
// (same key style as pad-theme). Falls back to the browser language when the
// prefix matches a known locale, else English.
export const LANGUAGE_KEY = 'pad-language'
const SUPPORTED = ['en'] // future locales join here

function detectLanguage(): string {
  try {
    const stored = localStorage.getItem(LANGUAGE_KEY)
    if (stored && SUPPORTED.includes(stored)) return stored
    const nav = navigator.language?.slice(0, 2)
    if (nav && SUPPORTED.includes(nav)) return nav
  } catch {
    /* storage unavailable — default below */
  }
  return 'en'
}

void i18next.use(initReactI18next).init({
  lng: detectLanguage(),
  fallbackLng: 'en',
  // Bundled resources, no backend plugin — init is synchronous, so no
  // Suspense boundary is required anywhere in the app.
  resources: {
    en: {
      common: en.common,
      shell: en.shell,
      findings: en.findings,
      auth: en.auth,
      settings: enSettings,
    },
  },
  defaultNS: 'common',
  ns: ['common', 'shell', 'findings', 'auth', 'settings'],
  interpolation: {
    // React already escapes interpolated values; i18next escaping is for
    // non-React targets and would double-encode (&amp; etc.).
    escapeValue: false,
  },
  react: {
    // Keys are stable; bind once per language change rather than per render.
    bindI18n: 'languageChanged',
  },
  returnEmptyString: false,
})

// Keep <html lang> in sync for screen readers and browser translation hints.
function syncHtmlLang(lng: string) {
  document.documentElement.lang = lng
}
syncHtmlLang(i18next.language)
i18next.on('languageChanged', syncHtmlLang)

export function setLanguage(lng: string) {
  try {
    localStorage.setItem(LANGUAGE_KEY, lng)
  } catch {
    /* non-fatal: in-memory language still switches */
  }
  void i18next.changeLanguage(lng)
}

export default i18next
