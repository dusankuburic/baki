import {useState, lazy, Suspense} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import Modal from '@/components/shared/Modal'
import Spinner from '@/components/shared/Spinner'
import {isTauri} from '@/platform/guards'

// Each panel is lazy: only one is visible at a time, so the settings chunk
// fetches a panel on first open of that section instead of shipping all 14
// (including ~46 kB of cloud-only panels a desktop user can never open) with
// the modal. The barrel (./index) stays for typed external re-exports.
const GeneralPanel = lazy(() => import('./GeneralPanel'))
const ParserPanel = lazy(() => import('./ParserPanel'))
const ProvidersPanel = lazy(() => import('./ProvidersPanel'))
const AIBehaviorPanel = lazy(() => import('./AIBehaviorPanel'))
const AIPromptsPanel = lazy(() => import('./AIPromptsPanel'))
const AppearancePanel = lazy(() => import('./AppearancePanel'))
const RulesPanel = lazy(() => import('./RulesPanel'))
const PolicyGatePanel = lazy(() => import('./PolicyGatePanel'))
const OrganizationsPanel = lazy(() => import('./OrganizationsPanel'))
const KnowledgeBasePanel = lazy(() => import('./KnowledgeBasePanel'))
const ApiTokensPanel = lazy(() => import('./ApiTokensPanel'))
const ShortcutsPanel = lazy(() => import('./ShortcutsPanel'))
const PrivacyPanel = lazy(() => import('./PrivacyPanel'))
const AboutPanel = lazy(() => import('./AboutPanel'))

type SettingsSection =
  | 'general'
  | 'parser'
  | 'accounts'
  | 'behavior'
  | 'prompts'
  | 'appearance'
  | 'analysis'
  | 'policies'
  | 'orgs'
  | 'knowledge'
  | 'tokens'
  | 'shortcuts'
  | 'privacy'
  | 'about'

// Organisations are a cloud-mode (multi-user) concept; the desktop app is
// single-user and has no notion of orgs, so hide that entry there.
const isCloud = !isTauri()

// Section labels resolve at render time (not module init) so they re-resolve
// on language change; the module-level constant only carries the ids.
const CLOUD_SECTION_IDS = ['policies', 'orgs', 'knowledge', 'tokens'] as const
const SECTION_IDS = [
  'general',
  'parser',
  'accounts',
  'behavior',
  'prompts',
  'appearance',
  'analysis',
  ...(isCloud ? CLOUD_SECTION_IDS : []),
  'shortcuts',
  'privacy',
  'about',
] as const

function useSections(): {id: SettingsSection; label: string}[] {
  const {t} = useTranslation('settings')
  return SECTION_IDS.map(id => ({id, label: t(`modal.sections.${id}`)}))
}

export default function SettingsModal({
  isOpen,
  onClose,
  initialSection,
}: {
  isOpen: boolean
  onClose: () => void
  // initialSection selects the opening tab (deep-link from onboarding /
  // missing-key states); falls back to 'general' when unknown.
  initialSection?: string | null
}) {
  const {t} = useTranslation('settings')
  const sections = useSections()
  const validInitial =
    initialSection && sections.some(s => s.id === initialSection) ? (initialSection as SettingsSection) : 'general'
  const [activeSection, setActiveSection] = useState<SettingsSection>(validInitial)

  return (
    <Modal isOpen={isOpen} onClose={onClose} title={t('modal.title')} size="xl" height="tall" bodyScroll={false}>
      <div className="flex h-full">
        {/* Sidebar */}
        <div className="w-48 border-r border-border-default px-4 py-3 overflow-y-auto shrink-0">
          <nav className="space-y-0.5">
            {sections.map(s => (
              <button
                key={s.id}
                onClick={() => setActiveSection(s.id)}
                className={clsx(
                  'w-full text-left px-3 py-2 rounded-md text-sm font-medium transition-colors',
                  activeSection === s.id
                    ? 'bg-brand-500/10 text-brand-500'
                    : 'text-text-secondary hover:bg-surface-3 hover:text-text-primary',
                )}
              >
                {s.label}
              </button>
            ))}
          </nav>
        </div>

        {/* Content — the single scroll region */}
        <div className="flex-1 p-6 overflow-y-auto min-h-0">
          <Suspense
            fallback={
              <div className="flex items-center justify-center py-16">
                <Spinner size={20} />
              </div>
            }
          >
            {activeSection === 'general' && <GeneralPanel />}
            {activeSection === 'parser' && <ParserPanel />}
            {activeSection === 'accounts' && <ProvidersPanel />}
            {activeSection === 'behavior' && <AIBehaviorPanel />}
            {activeSection === 'prompts' && <AIPromptsPanel />}
            {activeSection === 'appearance' && <AppearancePanel />}
            {activeSection === 'analysis' && <RulesPanel />}
            {activeSection === 'policies' && isCloud && <PolicyGatePanel />}
            {activeSection === 'orgs' && isCloud && <OrganizationsPanel />}
            {activeSection === 'knowledge' && isCloud && <KnowledgeBasePanel />}
            {activeSection === 'tokens' && isCloud && <ApiTokensPanel />}
            {activeSection === 'shortcuts' && <ShortcutsPanel />}
            {activeSection === 'privacy' && <PrivacyPanel />}
            {activeSection === 'about' && <AboutPanel />}
          </Suspense>
        </div>
      </div>
    </Modal>
  )
}
