import type {ReactNode} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import type {ThemeMode} from '@/types'
import {DARK_THEMES, LIGHT_THEMES, SYSTEM_THEME} from '@/lib/themeRegistry'
import {Check} from 'lucide-react'
import clsx from 'clsx'
import {useTranslation} from 'react-i18next'
import Switch from '@/components/shared/Switch'
import SettingRow from './SettingRow'

export default function AppearancePanel() {
  const {t} = useTranslation('settings')
  const {theme, density, reduceMotion, highContrast, chatTextSize} = useSettingsStore(s => s.settings.appearance)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)

  return (
    <div className="space-y-10">
      <div>
        <label className="text-sm font-medium text-text-primary block mb-4">{t('appearance.colorTheme')}</label>

        <div className="mb-6">
          <ThemeCard
            theme={SYSTEM_THEME}
            layout="row"
            isSelected={theme === SYSTEM_THEME.id}
            onClick={() => updateAppearance({theme: SYSTEM_THEME.id})}
          />
        </div>

        <ThemeSection label={t('appearance.darkThemes')} count={DARK_THEMES.length}>
          {DARK_THEMES.map(t => (
            <ThemeCard
              key={t.id}
              theme={t}
              isSelected={theme === t.id}
              onClick={() => updateAppearance({theme: t.id})}
            />
          ))}
        </ThemeSection>

        <ThemeSection label={t('appearance.lightThemes')} count={LIGHT_THEMES.length}>
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

      <div className="grid gap-8 grid-cols-[repeat(auto-fit,minmax(16rem,1fr))]">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">{t('appearance.densityLabel')}</label>
          <div className="flex gap-2">
            <DensityButton
              label={t('appearance.comfortable')}
              isActive={density === 'comfortable'}
              onClick={() => updateAppearance({density: 'comfortable'})}
            />
            <DensityButton
              label={t('appearance.compact')}
              isActive={density === 'compact'}
              onClick={() => updateAppearance({density: 'compact'})}
            />
          </div>
          <p className="text-xs text-text-tertiary mt-2">{t('appearance.densityHint')}</p>
        </div>

        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">{t('appearance.chatTextSize')}</label>
          <div className="flex gap-2">
            <DensityButton
              label={t('appearance.chatTextSm')}
              isActive={chatTextSize === 'sm'}
              onClick={() => updateAppearance({chatTextSize: 'sm'})}
            />
            <DensityButton
              label={t('appearance.chatTextMd')}
              isActive={chatTextSize === 'md'}
              onClick={() => updateAppearance({chatTextSize: 'md'})}
            />
            <DensityButton
              label={t('appearance.chatTextLg')}
              isActive={chatTextSize === 'lg'}
              onClick={() => updateAppearance({chatTextSize: 'lg'})}
            />
          </div>
          <p className="text-xs text-text-tertiary mt-2">{t('appearance.chatTextHint')}</p>
        </div>

        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">{t('appearance.accessibility')}</label>
          <div className="space-y-3">
            <SettingRow
              title={t('appearance.highContrast')}
              control={
                <Switch
                  label={t('appearance.highContrast')}
                  labelHidden
                  checked={highContrast}
                  onChange={v => updateAppearance({highContrast: v})}
                />
              }
            />
            <SettingRow
              title={t('appearance.reduceMotion')}
              control={
                <Switch
                  label={t('appearance.reduceMotion')}
                  labelHidden
                  checked={reduceMotion}
                  onChange={v => updateAppearance({reduceMotion: v})}
                />
              }
            />
          </div>
        </div>
      </div>
    </div>
  )
}

function ThemeSection({label, count, children}: {label: string; count: number; children: ReactNode}) {
  return (
    <div className="mb-6 last:mb-0">
      <div className="flex items-baseline gap-2 mb-3">
        <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary">{label}</h3>
        <span className="text-2xs text-text-disabled">{count}</span>
      </div>
      <div className="grid gap-4 grid-cols-[repeat(auto-fill,minmax(200px,1fr))]">{children}</div>
    </div>
  )
}

function ThemeCard({
  theme,
  isSelected,
  onClick,
  layout = 'card',
}: {
  theme: {id: ThemeMode; label: string; description: string}
  isSelected: boolean
  onClick: () => void
  layout?: 'card' | 'row'
}) {
  const row = layout === 'row'
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={isSelected}
      className={clsx(
        'group relative flex text-left rounded-xl border-2 transition-[background-color] duration-fast overflow-hidden bg-surface-1',
        row ? 'w-full flex-row items-stretch' : 'flex-col',
        isSelected
          ? 'border-brand-500 ring-4 ring-brand-500/10'
          : 'border-border-default hover:border-border-strong hover:bg-surface-2',
      )}
    >
      <div
        data-theme={theme.id}
        className={clsx(
          'relative overflow-hidden bg-surface-0 border-border-subtle',
          row ? 'h-16 w-32 shrink-0 border-r' : 'h-24 w-full border-b',
        )}
      >
        <div className="h-4 w-full bg-surface-1 border-b border-border-subtle flex items-center px-1.5 gap-1">
          <div className="w-1.5 h-1.5 rounded-full bg-brand-500 opacity-60" />
          <div className="w-8 h-1 rounded-full bg-text-tertiary opacity-20" />
        </div>
        <div className="flex h-full">
          <div className="w-6 h-full border-r border-border-subtle bg-surface-1 p-1 space-y-1">
            <div className="w-full h-1 rounded-full bg-text-secondary opacity-10" />
            <div className="w-2/3 h-1 rounded-full bg-text-secondary opacity-10" />
            <div className="w-full h-1 rounded-full bg-text-secondary opacity-10" />
          </div>
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

      <div className={clsx('min-w-0 p-3', row && 'flex flex-col justify-center')}>
        <div className="text-xs font-bold text-text-primary mb-0.5 uppercase tracking-wider">{theme.label}</div>
        <div className="text-2xs text-text-tertiary leading-tight line-clamp-1">{theme.description}</div>
      </div>
    </button>
  )
}

function DensityButton({label, isActive, onClick}: {label: string; isActive: boolean; onClick: () => void}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={isActive}
      className={clsx(
        'px-3 py-1.5 text-xs font-medium rounded-md border transition-colors flex-1',
        isActive
          ? 'bg-brand-500 border-brand-500 text-brand-foreground shadow-sm'
          : 'bg-surface-2 border-border-default text-text-secondary hover:bg-surface-3',
      )}
    >
      {label}
    </button>
  )
}
