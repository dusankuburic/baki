import {useState} from 'react'
import clsx from 'clsx'
import Modal from '@/components/shared/Modal'
import ProvidersPanel from './ProvidersPanel'
import AppearancePanel from './AppearancePanel'
import RulesPanel from './RulesPanel'
import ShortcutsPanel from './ShortcutsPanel'
import PrivacyPanel from './PrivacyPanel'
import AboutPanel from './AboutPanel'

type SettingsSection = 'providers' | 'appearance' | 'analysis' | 'shortcuts' | 'privacy' | 'about'

const sections: {id: SettingsSection; label: string}[] = [
  {id: 'providers', label: 'Providers'},
  {id: 'appearance', label: 'Appearance'},
  {id: 'analysis', label: 'Analysis'},
  {id: 'shortcuts', label: 'Shortcuts'},
  {id: 'privacy', label: 'Privacy'},
  {id: 'about', label: 'About'},
]

interface Props {
  isOpen: boolean
  onClose: () => void
}

export default function SettingsModal({isOpen, onClose}: Props) {
  const [activeSection, setActiveSection] = useState<SettingsSection>('providers')

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
          {activeSection === 'providers' && <ProvidersPanel />}
          {activeSection === 'appearance' && <AppearancePanel />}
          {activeSection === 'analysis' && <RulesPanel />}
          {activeSection === 'shortcuts' && <ShortcutsPanel />}
          {activeSection === 'privacy' && <PrivacyPanel />}
          {activeSection === 'about' && <AboutPanel />}
        </div>
      </div>
    </Modal>
  )
}
