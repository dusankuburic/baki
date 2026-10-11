import {Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {X} from 'lucide-react'
import Modal from '@/components/shared/Modal'
import IconButton from '@/components/shared/IconButton'
import Skeleton from '@/components/shared/Skeleton'
import ErrorBoundary from '@/components/shared/ErrorBoundary'
import {useUIStore, type SettingsSection} from '@/stores/uiStore'
import SettingsNav from './SettingsNav'
import {SETTINGS_PANELS, isCloudRuntime, visibleFooterSections, visibleGroups, visibleSectionIds} from './sections'

export default function SettingsModal({
  isOpen,
  onClose,
  initialSection,
}: {
  isOpen: boolean
  onClose: () => void
  initialSection?: SettingsSection | null
}) {
  const {t} = useTranslation('settings')
  const cloud = isCloudRuntime()
  const groups = useMemo(() => visibleGroups(cloud), [cloud])
  const footer = useMemo(() => visibleFooterSections(cloud), [cloud])
  const ids = useMemo(() => visibleSectionIds(cloud), [cloud])

  const [active, setActive] = useState<SettingsSection>(() => {
    const remembered = useUIStore.getState().lastSettingsSection
    return [initialSection, remembered].find((s): s is SettingsSection => !!s && ids.includes(s)) ?? 'general'
  })
  const setLastSettingsSection = useUIStore(s => s.setLastSettingsSection)
  useEffect(() => setLastSettingsSection(active), [active, setLastSettingsSection])

  useEffect(() => {
    if (useUIStore.getState().settingsSection) useUIStore.getState().setSettingsSection(null)
    return useUIStore.subscribe((s, prev) => {
      const section = s.settingsSection
      if (!section || section === prev.settingsSection) return
      if (ids.includes(section)) setActive(section)
      s.setSettingsSection(null)
    })
  }, [ids])

  const selectSection = useCallback((section: SettingsSection) => {
    if (useUIStore.getState().settingsAnchor) useUIStore.getState().clearSettingsAnchor()
    setActive(section)
  }, [])

  const [query, setQuery] = useState('')
  const selectedTabRef = useRef<HTMLButtonElement>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = 0
  }, [active])

  const Panel = SETTINGS_PANELS[active]

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      ariaLabel={t('modal.ui.title')}
      size="2xl"
      height="fill"
      bodyScroll={false}
      className="settings-dialog"
      viewTransitionName="settings"
      initialFocusRef={selectedTabRef}
    >
      <div className="settings-shell">
        <SettingsNav
          groups={groups}
          footer={footer}
          active={active}
          onSelect={selectSection}
          query={query}
          onQueryChange={setQuery}
          selectedTabRef={selectedTabRef}
        />
        <section className="settings-content">
          <header className="settings-content-header flex items-start gap-4 border-b border-border-subtle px-8 pb-4 pt-5">
            <div className="min-w-0 flex-1">
              <h2 id="settings-heading" className="truncate text-xl font-semibold text-text-primary">
                {t(`modal.sections.${active}.label`)}
              </h2>
              <p className="mt-1 text-sm text-text-secondary">{t(`modal.sections.${active}.description`)}</p>
            </div>
            <IconButton icon={X} label={t('modal.ui.close')} onClick={onClose} />
          </header>
          <div
            ref={scrollRef}
            id="settings-panel"
            role="tabpanel"
            aria-labelledby="settings-heading"
            tabIndex={0}
            className="settings-content-body px-8 pb-10 pt-6"
          >
            <ErrorBoundary key={active}>
              <Suspense fallback={<PanelFallback label={t('modal.ui.loading')} />}>
                <Panel />
              </Suspense>
            </ErrorBoundary>
          </div>
        </section>
      </div>
    </Modal>
  )
}

function PanelFallback({label}: {label: string}) {
  return (
    <div role="status" aria-label={label} className="space-y-6">
      <Skeleton lines={2} className="max-w-md" />
      <div className="h-28 rounded-lg bg-surface-2 animate-pulse-soft" />
      <div className="h-28 rounded-lg bg-surface-2 animate-pulse-soft" />
    </div>
  )
}
