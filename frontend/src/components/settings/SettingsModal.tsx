import {useState} from 'react'
import clsx from 'clsx'
import Modal from '@/components/shared/Modal'
import {isTauri} from '@/platform/guards'
import {
  GeneralPanel,
  ProvidersPanel,
  AIBehaviorPanel,
  AIPromptsPanel,
  AppearancePanel,
  ParserPanel,
  RulesPanel,
  OrganizationsPanel,
  KnowledgeBasePanel,
  ApiTokensPanel,
  ShortcutsPanel,
  PrivacyPanel,
  AboutPanel,
} from './index'

type SettingsSection =
  | 'general'
  | 'parser'
  | 'accounts'
  | 'behavior'
  | 'prompts'
  | 'appearance'
  | 'analysis'
  | 'orgs'
  | 'knowledge'
  | 'tokens'
  | 'shortcuts'
  | 'privacy'
  | 'about'

// Organisations are a cloud-mode (multi-user) concept; the desktop app is
// single-user and has no notion of orgs, so hide that entry there.
const isCloud = !isTauri()

const sections: {id: SettingsSection; label: string}[] = [
  {id: 'general', label: 'General'},
  {id: 'parser', label: 'Parser'},
  {id: 'accounts', label: 'AI Accounts'},
  {id: 'behavior', label: 'AI Behavior'},
  {id: 'prompts', label: 'AI Prompts'},
  {id: 'appearance', label: 'Appearance'},
  {id: 'analysis', label: 'Analysis'},
  ...(isCloud
    ? [
        {id: 'orgs' as const, label: 'Organizations'},
        {id: 'knowledge' as const, label: 'Knowledge Base'},
        {id: 'tokens' as const, label: 'API Tokens'},
      ]
    : []),
  {id: 'shortcuts', label: 'Shortcuts'},
  {id: 'privacy', label: 'Privacy'},
  {id: 'about', label: 'About'},
]

export default function SettingsModal({isOpen, onClose}: {isOpen: boolean; onClose: () => void}) {
  const [activeSection, setActiveSection] = useState<SettingsSection>('general')

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Settings" size="xl" height="tall" bodyScroll={false}>
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
          {activeSection === 'general' && <GeneralPanel />}
          {activeSection === 'parser' && <ParserPanel />}
          {activeSection === 'accounts' && <ProvidersPanel />}
          {activeSection === 'behavior' && <AIBehaviorPanel />}
          {activeSection === 'prompts' && <AIPromptsPanel />}
          {activeSection === 'appearance' && <AppearancePanel />}
          {activeSection === 'analysis' && <RulesPanel />}
          {activeSection === 'orgs' && isCloud && <OrganizationsPanel />}
          {activeSection === 'knowledge' && isCloud && <KnowledgeBasePanel />}
          {activeSection === 'tokens' && isCloud && <ApiTokensPanel />}
          {activeSection === 'shortcuts' && <ShortcutsPanel />}
          {activeSection === 'privacy' && <PrivacyPanel />}
          {activeSection === 'about' && <AboutPanel />}
        </div>
      </div>
    </Modal>
  )
}
