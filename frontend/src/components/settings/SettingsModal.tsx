import {Suspense, useEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {X} from 'lucide-react'
import Modal from '@/components/shared/Modal'
import IconButton from '@/components/shared/IconButton'
import Skeleton from '@/components/shared/Skeleton'
import ErrorBoundary from '@/components/shared/ErrorBoundary'
import {useUIStore, type SettingsSection} from '@/stores/uiStore'
import SettingsNav from './SettingsNav'
import {SETTINGS_PANELS, isCloudRuntime, visibleFooterSections, visibleGroups, visibleSectionIds} from './sections'

// The Settings dialog: a fixed 1200×960 frame (viewport-capped, see
// index.css "SETTINGS MODAL") with the section sidebar on the left and, on
// the right, a header naming the active section above the one scroll region
// that holds its panel. Sections, groups and panels come from ./sections.

export default function SettingsModal({
  isOpen,
  onClose,
  initialSection,
}: {
  isOpen: boolean
  onClose: () => void
  // Deep link (onboarding, missing-key states, profile links). Wins over the
  // remembered section; an unknown or hidden id falls back to General.
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

  const [query, setQuery] = useState('')
  const selectedTabRef = useRef<HTMLButtonElement>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  // The scroll container outlives the panels, so a new section would
  // otherwise open at the previous one's scroll offset.
  useEffect(() => {
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
          onSelect={setActive}
          query={query}
          onQueryChange={setQuery}
          selectedTabRef={selectedTabRef}
        />
        <section className="settings-content">
          {/* Named from the registry, so the heading is there before the
              panel chunk has arrived. */}
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
            {/* Keyed on the section so a panel that threw is retried fresh
                when the user moves on; the boundary keeps the failure inside
                the body instead of unmounting the whole dialog. */}
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

// Shaped like a typical panel (a text block and two cards) so the body does
// not jump when the real one arrives; the frame itself never moves.
function PanelFallback({label}: {label: string}) {
  return (
    <div role="status" aria-label={label} className="space-y-6">
      <Skeleton lines={2} className="max-w-md" />
      <div className="h-28 rounded-lg bg-surface-2 animate-pulse-soft" />
      <div className="h-28 rounded-lg bg-surface-2 animate-pulse-soft" />
    </div>
  )
}
