import {useState} from 'react'
import clsx from 'clsx'
import Modal from '@/components/shared/Modal'
import {isTauri} from '@/platform/guards'
import {
  GeneralPanel,
  ProvidersPanel,
  AIBehaviorPanel,
  AppearancePanel,
  ParserPanel,
  RulesPanel,
  OrganizationsPanel,
  ShortcutsPanel,
  PrivacyPanel,
  AboutPanel
} from './index'

type SettingsSection = 'general' | 'parser' | 'accounts' | 'behavior' | 'appearance' | 'analysis' | 'orgs' | 'shortcuts' | 'privacy' | 'about'

// Organisations are a cloud-mode (multi-user) concept; the desktop app is
// single-user and has no notion of orgs, so hide that entry there.
const isCloud = !isTauri()

const sections: {id: SettingsSection; label: string}[] = [
  {id: 'general', label: 'General'},
  {id: 'parser', label: 'Parser'},
  {id: 'accounts', label: 'AI Accounts'},
  {id: 'behavior', label: 'AI Behavior'},
  {id: 'appearance', label: 'Appearance'},
  {id: 'analysis', label: 'Analysis'},
  ...(isCloud ? [{id: 'orgs' as const, label: 'Organizations'}] : []),
  {id: 'shortcuts', label: 'Shortcuts'},
  {id: 'privacy', label: 'Privacy'},
  {id: 'about', label: 'About'},
]

interface Props {
  isOpen: boolean
  onClose: () => void
}

export default function SettingsModal({isOpen, onClose}: Props) {
  const [activeSection, setActiveSection] = useState<SettingsSection>('general')

  return (
    <Modal isOpen={isOpen} onClose={onClose} size="xl" closeOnEsc={true}>
      <div className="flex" style={{minHeight: 480, margin: '-16px -24px'}}>
        <nav className="w-[200px] bg-surface-2 border-r border-border-default py-2 shrink-0">
          {sections.map(s => (
            <button
              key={s.id}
              onClick={() => setActiveSection(s.id)}
              className={clsx(
                'w-full text-left px-4 py-2.5 text-sm transition-colors',
                activeSection === s.id
                  ? 'bg-brand-500/10 border-l-2 border-brand-500 text-text-primary'
                  : 'text-text-secondary hover:bg-surface-3 border-l-2 border-transparent',
              )}
            >
              {s.label}
            </button>
          ))}
        </nav>
        <div className="flex-1 p-6 overflow-y-auto max-h-[70vh]">
          {activeSection === 'general' && <GeneralPanel />}
          {activeSection === 'parser' && <ParserPanel />}
          {activeSection === 'accounts' && <ProvidersPanel />}
          {activeSection === 'behavior' && <AIBehaviorPanel />}
          {activeSection === 'appearance' && <AppearancePanel />}
          {activeSection === 'analysis' && <RulesPanel />}
          {activeSection === 'orgs' && isCloud && <OrganizationsPanel />}
          {activeSection === 'shortcuts' && <ShortcutsPanel />}
          {activeSection === 'privacy' && <PrivacyPanel />}
          {activeSection === 'about' && <AboutPanel />}
        </div>
      </div>
    </Modal>
  )
}
