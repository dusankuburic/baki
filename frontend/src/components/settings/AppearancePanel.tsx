import type {ReactNode} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import type {ThemeMode} from '@/types'
import {DARK_THEMES, LIGHT_THEMES, SYSTEM_THEME} from '@/lib/themeRegistry'
import {Check} from 'lucide-react'
import clsx from 'clsx'

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

          {/* System (follows OS) — full width */}
          <div className="mb-6">
            <ThemeCard
              theme={SYSTEM_THEME}
              isSelected={theme === SYSTEM_THEME.id}
              onClick={() => updateAppearance({theme: SYSTEM_THEME.id})}
            />
          </div>

          {/* Dark themes */}
          <ThemeSection label="Dark Themes" count={DARK_THEMES.length}>
            {DARK_THEMES.map(t => (
              <ThemeCard
                key={t.id}
                theme={t}
                isSelected={theme === t.id}
                onClick={() => updateAppearance({theme: t.id})}
              />
            ))}
          </ThemeSection>

          {/* Light themes */}
          <ThemeSection label="Light Themes" count={LIGHT_THEMES.length}>
            {LIGHT_THEMES.map(t => (
              <ThemeCard
                key={t.id}
                theme={t}
                isSelected={theme === t.id}
                onClick={() => updateAppearance({theme: t.id})}
              />
            ))}
          </ThemeSection>
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

function ThemeSection({label, count, children}: {label: string, count: number, children: ReactNode}) {
  return (
    <div className="mb-6 last:mb-0">
      <div className="flex items-baseline gap-2 mb-3">
        <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary">{label}</h3>
        <span className="text-2xs text-text-disabled">{count}</span>
      </div>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {children}
      </div>
    </div>
  )
}

function ThemeCard({theme, isSelected, onClick}: {theme: {id: ThemeMode, label: string, description: string}, isSelected: boolean, onClick: () => void}) {
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
            <div className="w-1/2 h-1.5 rounded-sm bg-brand-500 opacity-20 shadow-[0_0_8px_var(--brand-500)]" />
          </div>
        </div>

        {isSelected && (
          <div className="absolute top-2 right-2 bg-brand-500 text-brand-foreground rounded-full p-0.5 shadow-lg">
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
          ? 'bg-brand-500 border-brand-500 text-brand-foreground shadow-sm'
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
          'absolute top-0.5 w-3 h-3 rounded-full transition-all duration-fast shadow-sm',
          isChecked ? 'right-0.5 bg-brand-foreground' : 'left-0.5 bg-white'
        )} />
      </button>
    </div>
  )
}
