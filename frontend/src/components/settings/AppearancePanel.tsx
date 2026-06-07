import {useSettingsStore} from '@/stores/settingsStore'
import type {ThemeMode} from '@/types/domain'
import {Check} from 'lucide-react'
import clsx from 'clsx'

const themes: {id: ThemeMode; label: string; description: string}[] = [
  {id: 'dark',        label: 'Deep Dark',   description: 'The standard high-contrast dark mode.'},
  {id: 'light',       label: 'Clean Light',  description: 'Bright and airy for high-light environments.'},
  {id: 'midnight',    label: 'Midnight',    description: 'A cool, deep navy palette for night owls.'},
  {id: 'tokyo-night', label: 'Tokyo Night', description: 'Deep purples and neon cyan accents.'},
  {id: 'one-dark',    label: 'One Dark',    description: 'Classic soft gray with balanced contrast.'},
  {id: 'dracula',     label: 'Dracula',     description: 'Vibrant colors on a vampiric purple base.'},
  {id: 'nord',        label: 'Nord',        description: 'An elegant arctic-bluish clean aesthetic.'},
  {id: 'warm',        label: 'Warm Sand',    description: 'A soft, sepia-toned dark theme.'},
  {id: 'system',      label: 'System',      description: 'Automatically follow your OS preferences.'},
]

export default function AppearancePanel() {
  const {theme, density, reduceMotion, highContrast} = useSettingsStore(s => s.settings.appearance)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)

  return (
    <div className="pb-8">
      <h2 className="text-xl font-semibold text-text-primary">Appearance</h2>
      <p className="text-sm text-text-secondary mt-1 mb-8">
        Customize the look and feel of PAD Analyzer.
      </p>

      <div className="space-y-10">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-4">Color Theme</label>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
            {themes.map(t => (
              <ThemeCard 
                key={t.id}
                theme={t}
                isSelected={theme === t.id}
                onClick={() => updateAppearance({theme: t.id})}
              />
            ))}
          </div>
        </div>

        {/* ... Rest of settings (density, accessibility) ... */}
        <div className="grid grid-cols-1 md:grid-cols-2 gap-8">
          <div>
            <label className="text-sm font-medium text-text-primary block mb-3">UI Density</label>
            <div className="flex gap-2">
              <DensityButton 
                label="Comfortable" 
                isActive={density === 'comfortable'} 
                onClick={() => updateAppearance({density: 'comfortable'})} 
              />
              <DensityButton 
                label="Compact" 
                isActive={density === 'compact'} 
                onClick={() => updateAppearance({density: 'compact'})} 
              />
            </div>
            <p className="text-xs text-text-tertiary mt-2">
              Compact shows more content, comfortable adds spacing.
            </p>
          </div>

          <div className="space-y-4">
            <label className="text-sm font-medium text-text-primary block">Accessibility</label>
            <div className="space-y-3">
              <AccessibilityToggle 
                label="High Contrast" 
                isChecked={highContrast} 
                onChange={v => updateAppearance({highContrast: v})} 
              />
              <AccessibilityToggle 
                label="Reduce Motion" 
                isChecked={reduceMotion} 
                onChange={v => updateAppearance({reduceMotion: v})} 
              />
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function ThemeCard({theme, isSelected, onClick}: {theme: typeof themes[0], isSelected: boolean, onClick: () => void}) {
  return (
    <button
      onClick={onClick}
      className={clsx(
        'group relative flex flex-col text-left rounded-xl border-2 transition-all duration-fast overflow-hidden bg-surface-1',
        isSelected 
          ? 'border-brand-500 ring-4 ring-brand-500/10' 
          : 'border-border-default hover:border-border-strong hover:bg-surface-2'
      )}
    >
      {/* Mock UI Preview */}
      <div 
        data-theme={theme.id} 
        className="h-24 w-full border-b border-border-subtle bg-surface-0 relative overflow-hidden"
      >
        {/* Mock Header */}
        <div className="h-4 w-full bg-surface-1 border-b border-border-subtle flex items-center px-1.5 gap-1">
          <div className="w-1.5 h-1.5 rounded-full bg-brand-500 opacity-60" />
          <div className="w-8 h-1 rounded-full bg-text-tertiary opacity-20" />
        </div>
        <div className="flex h-full">
          {/* Mock Sidebar */}
          <div className="w-6 h-full border-r border-border-subtle bg-surface-1 p-1 space-y-1">
            <div className="w-full h-1 rounded-full bg-text-secondary opacity-10" />
            <div className="w-2/3 h-1 rounded-full bg-text-secondary opacity-10" />
            <div className="w-full h-1 rounded-full bg-text-secondary opacity-10" />
          </div>
          {/* Mock Content */}
          <div className="flex-1 p-2 space-y-2">
            <div className="space-y-1">
              <div className="w-3/4 h-1.5 rounded-sm bg-block-action opacity-30" />
              <div className="w-full h-1.5 rounded-sm bg-block-condition opacity-30" />
            </div>
            <div className="w-1/2 h-1.5 rounded-sm bg-brand-500 opacity-40 shadow-[0_0_8px_var(--brand-500)]" style={{opacity: 0.2}} />
          </div>
        </div>

        {isSelected && (
          <div className="absolute top-2 right-2 bg-brand-500 text-white rounded-full p-0.5 shadow-lg">
            <Check className="w-3 h-3" strokeWidth={3} />
          </div>
        )}
      </div>

      <div className="p-3">
        <div className="text-xs font-bold text-text-primary mb-0.5 uppercase tracking-wider">{theme.label}</div>
        <div className="text-2xs text-text-tertiary leading-tight line-clamp-1">{theme.description}</div>
      </div>
    </button>
  )
}

function DensityButton({label, isActive, onClick}: {label: string, isActive: boolean, onClick: () => void}) {
  return (
    <button
      onClick={onClick}
      className={clsx(
        'px-3 py-1.5 text-xs font-medium rounded-md border transition-colors flex-1',
        isActive 
          ? 'bg-brand-500 border-brand-500 text-white shadow-sm' 
          : 'bg-surface-2 border-border-default text-text-secondary hover:bg-surface-3'
      )}
    >
      {label}
    </button>
  )
}

function AccessibilityToggle({label, isChecked, onChange}: {label: string, isChecked: boolean, onChange: (v: boolean) => void}) {
  return (
    <div className="flex items-center justify-between px-3 py-2 rounded-lg bg-surface-2 border border-border-subtle">
      <span className="text-xs text-text-secondary">{label}</span>
      <button 
        onClick={() => onChange(!isChecked)}
        className={clsx(
          'w-7 h-4 rounded-full relative transition-colors',
          isChecked ? 'bg-brand-500' : 'bg-surface-4'
        )}
      >
        <div className={clsx(
          'absolute top-0.5 w-3 h-3 bg-white rounded-full transition-all duration-fast shadow-sm',
          isChecked ? 'right-0.5' : 'left-0.5'
        )} />
      </button>
    </div>
  )
}
