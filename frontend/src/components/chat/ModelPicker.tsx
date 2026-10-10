import {useTranslation} from 'react-i18next'
import {useCallback, useEffect, useId, useMemo, useRef, useState, type ComponentType} from 'react'
import clsx from 'clsx'
import {Check, ChevronDown, Cloud, HardDrive, Search, Settings, Users} from 'lucide-react'
import {GithubMark} from '@/components/shared'
import {Badge, Select} from '@/components/shared'
import {AZURE_SECTION_ANCHOR} from '@/components/settings/azure/registryView'
import {useAnchoredPosition} from '@/hooks/useAnchoredPosition'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {formatContextWindow} from '@/lib/format'
import {isImeKeydown} from '@/lib/imeKey'
import {groupModels, isPickerModel, isRegistryEntry} from '@/lib/modelGroups'
import {modelKind} from '@/lib/modelIds'
import {modelKindLabelKey, purposeLabel, purposeLabelKey, type PurposeLabelOf} from '@/lib/modelPurpose'
import {matchesQuery} from '@/lib/textMatch'
import {PROVIDERS, isKnownProviderId, type ModelDetail} from '@/types'

/** A configured provider as the picker lists it. */
export interface PickerProvider {
  id: string
  name: string
  /** Its credentials come from the listing scope's organisation. */
  shared?: boolean
  /** The catalogue's glyph name (ProviderSpec.icon); the id map applies without one. */
  icon?: string
  models: ModelDetail[]
}

interface Props {
  /** The configured providers; the Provider select appears when there is more than one. */
  providers: PickerProvider[]
  /** The thread's provider, model ('' = nothing usable) and the model's name. */
  provider: string
  model: string
  label: string
  /** The thread model's listing entry (purpose, kind, shared), when listed. */
  modelDetail?: ModelDetail
  /** Per provider id: the model its new chats start on here (the [DEFAULT] badge). */
  defaults: Record<string, string>
  /** A pick: pins the thread to this provider's model. */
  onSelect: (providerId: string, model: ModelDetail) => void
  /** The listing scope's organisation, named by "Shared" badges. */
  orgName?: string
  /** Show "Your project has models you haven't added · Review" (ProviderInfo.suggestDiscovery). */
  reviewPrompt?: boolean
  /** No thread to pin a pick to: the trigger only shows the model. */
  disabled?: boolean
  /** Rendered first inside the trigger (the connection dot), with its hover title. */
  leading?: React.ReactNode
  title?: string
  className?: string
  /** Also receives the trigger (the chat moves focus to it after a notice is dismissed). */
  triggerRef?: React.MutableRefObject<HTMLButtonElement | null>
}

// More models than this in one provider and the popup offers a search box.
export const PICKER_SEARCH_THRESHOLD = 7
// The AI Accounts heading "Manage models…" lands on for Azure AI Foundry
// (the panel owns the id).
export const AZURE_ACCOUNTS_ANCHOR = AZURE_SECTION_ANCHOR

type StatusKey =
  | 'modelStatus.needs_credentials'
  | 'modelStatus.unreadable'
  | 'modelStatus.invalid_config'
  | 'modelStatus.unsupported'
  | 'modelStatus.unknown'

// Why an entry cannot be used right now; null when it can.
function statusKey(status: string | undefined): StatusKey | null {
  switch (status) {
    case undefined:
    case '':
    case 'ready':
      return null
    case 'needs_credentials':
    case 'unreadable':
    case 'invalid_config':
    case 'unsupported':
      return `modelStatus.${status}`
    default:
      return 'modelStatus.unknown'
  }
}

// The glyphs a catalogue entry may name (ProviderSpec.icon).
const PROVIDER_ICONS: Record<string, ComponentType<{size?: number}>> = {
  cloud: Cloud,
  'hard-drive': HardDrive,
  github: GithubMark,
}

// The provider's glyph in the trigger (its name is the tooltip and part of
// the trigger's accessible name): the catalogue's icon when the listing
// names one the bundle has, else by id.
function ProviderGlyph({id, icon}: {id: string; icon?: string}) {
  // An own key only: a name such as `constructor` must not resolve to Object.prototype.
  const Icon = icon && Object.prototype.hasOwnProperty.call(PROVIDER_ICONS, icon) ? PROVIDER_ICONS[icon] : undefined
  if (Icon) return <Icon size={12} />
  if (id === 'local') return <HardDrive size={12} />
  if (id === 'github-models' || id === 'copilot') return <GithubMark size={12} />
  return <Cloud size={12} />
}

interface GroupView {
  key: string
  label: string
  models: ModelDetail[]
}

// ModelPicker is the chat header's model control: each thread keeps its own
// model, and this is where it is chosen.
//
// The trigger names the thread's model first — at every width, the default
// 320 px chat included — with the provider as an icon (its name in the
// tooltip and the accessible name) and a Shared badge.
//
// The popup is a NON-modal dialog: it closes when focus leaves it. In Tab
// order: the Provider select (more than one configured provider; it only
// filters the list — nothing is pinned until a model is chosen), the search
// box (more than PICKER_SEARCH_THRESHOLD models; a combobox driving the
// listbox through aria-activedescendant), the grouped listbox (arrows move
// between options, never onto group headers; Enter picks), then "Manage
// models…". The first Escape clears the search, the next closes and returns
// focus to the trigger — both claimed (preventDefault + stopPropagation),
// so the chat's own Escape (stop the reply, leave focus mode) never fires.
export default function ModelPicker({
  providers,
  provider,
  model,
  label,
  modelDetail,
  defaults,
  onSelect,
  orgName,
  reviewPrompt,
  disabled,
  leading,
  title,
  className,
  triggerRef: outerTriggerRef,
}: Props) {
  const {t} = useTranslation('chat')
  const {t: tc} = useTranslation('common')
  const baseId = useId()
  const dialogId = `${baseId}-dialog`
  const listId = `${baseId}-list`
  const providerSelectId = `${baseId}-provider`

  const [open, setOpen] = useState(false)
  // The provider whose models the list shows (the Provider select).
  const [shownProvider, setShownProvider] = useState(provider)
  const [query, setQuery] = useState('')
  // The active option (aria-activedescendant), by model id.
  const [activeId, setActiveId] = useState<string | null>(null)

  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const setTrigger = useCallback(
    (el: HTMLButtonElement | null) => {
      triggerRef.current = el
      if (outerTriggerRef) outerTriggerRef.current = el
    },
    [outerTriggerRef],
  )
  const dialogRef = useRef<HTMLDivElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const blurTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const position = useAnchoredPosition({open, anchorRef: triggerRef, floatingRef: dialogRef, align: 'start'})

  const labelOf: PurposeLabelOf = useCallback(p => tc(purposeLabelKey(p)), [tc])

  const current = providers.find(p => p.id === shownProvider) ?? providers[0]
  const pickable = useMemo(() => (current?.models ?? []).filter(isPickerModel), [current])
  const searchable = pickable.length > PICKER_SEARCH_THRESHOLD

  const groups: GroupView[] = useMemo(() => {
    const all = groupModels(pickable, {labelOf}).map(g => ({
      key: `${g.kind}:${g.key}`,
      label: g.kind === 'preset' ? labelOf(g.key) : g.kind === 'custom' ? g.label : t('picker.plainGroup'),
      models: g.models,
    }))
    const q = searchable ? query.trim() : ''
    if (!q) return all
    return all
      .map(g => ({
        ...g,
        models: g.models.filter(m =>
          matchesQuery([m.displayName, m.id, m.description ?? '', m.connectionName ?? '', g.label].join('\n'), q),
        ),
      }))
      .filter(g => g.models.length > 0)
  }, [pickable, labelOf, t, searchable, query])

  const options = useMemo(() => groups.flatMap(g => g.models), [groups])
  // Each option's place in `options` (the arrow-key order), by entry.
  const indexOf = useMemo(() => new Map(options.map((m, i) => [m, i])), [options])
  const activeIndex = activeId === null ? -1 : options.findIndex(m => m.id === activeId)
  const optionId = (index: number) => `${baseId}-option-${index}`
  const activeDescendant = activeIndex >= 0 ? optionId(activeIndex) : undefined

  // ---- open / close ----

  const close = useCallback((restoreFocus: boolean) => {
    if (blurTimerRef.current !== null) {
      clearTimeout(blurTimerRef.current)
      blurTimerRef.current = null
    }
    setOpen(false)
    setQuery('')
    if (restoreFocus) triggerRef.current?.focus()
  }, [])

  const openPicker = useCallback(() => {
    if (disabled) return
    // The list starts on the thread's own provider, on its model.
    const own = providers.some(p => p.id === provider)
    setShownProvider(own ? provider : (providers[0]?.id ?? provider))
    setActiveId(own && model ? model : null)
    setQuery('')
    setOpen(true)
  }, [disabled, providers, provider, model])

  // Focus lands in the popup once it is on screen: the search box, else the
  // list, else — a provider with nothing to pick — its first control (the
  // Provider select, "Add models…", "Manage models…"), else the dialog
  // itself. Never left outside: the popup's own Escape and close-on-focus-out
  // only work with focus inside it (else Escape reached the chat's, which
  // stopped the reply or left focus mode).
  useEffect(() => {
    if (!open) return
    const dialog = dialogRef.current
    const target =
      searchRef.current ?? listRef.current ?? dialog?.querySelector<HTMLElement>('select, button') ?? dialog
    target?.focus({preventScroll: true})
  }, [open])

  // The popup's height follows what it lists: re-anchor on every change.
  const {update: reposition} = position
  useEffect(() => {
    if (open) reposition()
  }, [open, reposition, groups, shownProvider])

  // Keep the active option in view.
  useEffect(() => {
    if (!open || !activeDescendant) return
    document.getElementById(activeDescendant)?.scrollIntoView({block: 'nearest'})
  }, [open, activeDescendant])

  // "Choose model" (a notice) and "Choose another model" (an error bubble)
  // ask for the picker through the chat store; a request made while this
  // picker was not mounted is not replayed. Only a request opens it: the
  // counter going back to 0 is the store's logout reset.
  useEffect(
    () =>
      useChatStore.subscribe((state, prev) => {
        if (state.modelPickerRequest > prev.modelPickerRequest) openPicker()
      }),
    [openPicker],
  )

  // A press outside both the trigger and the popup closes it (focus goes
  // where the press lands).
  useEffect(() => {
    if (!open) return
    const onPointerDown = (e: MouseEvent) => {
      const target = e.target as Node
      if (dialogRef.current?.contains(target) || triggerRef.current?.contains(target)) return
      close(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    return () => document.removeEventListener('mousedown', onPointerDown)
  }, [open, close])

  useEffect(
    () => () => {
      if (blurTimerRef.current !== null) clearTimeout(blurTimerRef.current)
    },
    [],
  )

  // Non-modal: focus leaving the popup closes it. With no new focus target
  // (a press on the page, or the window losing focus) it waits a tick: a
  // window that lost focus keeps the popup's element active, and the popup
  // stays.
  const onDialogBlur = (e: React.FocusEvent<HTMLDivElement>) => {
    const next = e.relatedTarget as Node | null
    if (next && dialogRef.current?.contains(next)) return
    if (next) {
      close(false)
      return
    }
    if (blurTimerRef.current !== null) clearTimeout(blurTimerRef.current)
    blurTimerRef.current = setTimeout(() => {
      blurTimerRef.current = null
      const active = document.activeElement
      if (active && dialogRef.current?.contains(active)) return
      close(false)
    }, 0)
  }

  const onDialogKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Escape' || isImeKeydown(e)) return
    // Claimed either way, so neither the chat's Escape nor an enclosing
    // dialog (focus mode) acts on it.
    e.preventDefault()
    e.stopPropagation()
    if (searchable && query) {
      setQuery('')
      setActiveId(null)
      return
    }
    close(true)
  }

  // ---- choosing ----

  const choose = (m: ModelDetail) => {
    if (statusKey(m.status)) return
    onSelect(current?.id ?? shownProvider, m)
    close(true)
  }

  // "Manage models…": the popup closes and focus is back on the trigger
  // BEFORE Settings opens, so Settings restores focus to an element that is
  // still there when it closes.
  const manage = () => {
    const target = current?.id === 'azure-foundry' ? AZURE_ACCOUNTS_ANCHOR : undefined
    close(true)
    useUIStore.getState().openSettings('accounts', target)
  }

  const moveActive = (to: 'next' | 'prev' | 'first' | 'last') => {
    if (options.length === 0) return
    let index: number
    if (to === 'first') index = 0
    else if (to === 'last') index = options.length - 1
    else if (activeIndex < 0) index = to === 'next' ? 0 : options.length - 1
    else index = Math.min(options.length - 1, Math.max(0, activeIndex + (to === 'next' ? 1 : -1)))
    const opt = options[index]
    if (opt) setActiveId(opt.id)
  }

  const onSearchKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (isImeKeydown(e)) return
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      moveActive(e.key === 'ArrowDown' ? 'next' : 'prev')
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const opt = activeIndex >= 0 ? options[activeIndex] : undefined
      if (opt) choose(opt)
    }
  }

  const onListKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (isImeKeydown(e)) return
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      moveActive(e.key === 'ArrowDown' ? 'next' : 'prev')
    } else if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault()
      moveActive(e.key === 'Home' ? 'first' : 'last')
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      const opt = activeIndex >= 0 ? options[activeIndex] : undefined
      if (opt) choose(opt)
    }
  }

  const onListFocus = () => {
    if (activeIndex < 0 && options.length > 0) setActiveId(options[0]?.id ?? null)
  }

  const onTriggerKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowDown' && !open) {
      e.preventDefault()
      openPicker()
    }
  }

  // ---- the trigger ----

  const threadProvider = providers.find(p => p.id === provider)
  // A provider not configured here (the thread's own, while it falls back)
  // is not in the list: name it from the catalogue.
  const providerName = threadProvider?.name || (isKnownProviderId(provider) ? PROVIDERS[provider].label : provider)
  // Only a registry entry has a purpose (a catalogue model has none).
  const purpose = modelDetail && isRegistryEntry(modelDetail) ? purposeLabel(modelDetail, labelOf) : undefined
  const sharedModel = !!modelDetail?.shared
  const sharedCredentials = !sharedModel && !!threadProvider?.shared
  const org = orgName || t('picker.yourOrg')
  const triggerName = !label
    ? t('picker.triggerNameNone', {provider: providerName})
    : sharedModel
      ? purpose
        ? t('picker.triggerNamePurposeShared', {model: label, purpose, provider: providerName, org})
        : t('picker.triggerNameShared', {model: label, provider: providerName, org})
      : sharedCredentials
        ? t('picker.triggerNameSharedCredentials', {model: label, provider: providerName})
        : purpose
          ? t('picker.triggerNamePurpose', {model: label, purpose, provider: providerName})
          : t('picker.triggerName', {model: label, provider: providerName})

  const sharedBadge = (sharedModel || sharedCredentials) && (
    <Badge
      tone="neutral"
      icon={Users}
      srLabel={sharedModel ? t('modelBadge.sharedSr', {org}) : t('modelBadge.sharedCredentialsSr')}
      title={sharedModel ? t('modelBadge.sharedSr', {org}) : t('modelBadge.sharedCredentialsSr')}
    >
      {t('modelBadge.shared')}
    </Badge>
  )

  // ---- the list ----

  const isAzure = current?.id === 'azure-foundry'

  return (
    <div className={clsx('min-w-0', className)}>
      <button
        ref={setTrigger}
        type="button"
        className="flex items-center gap-1.5 w-full min-w-0 min-h-6 px-2 py-1 rounded-lg text-xs hover:bg-surface-2 transition-colors disabled:hover:bg-transparent"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? dialogId : undefined}
        aria-label={triggerName}
        title={title}
        disabled={disabled}
        // A press on the trigger while open must not blur the popup first
        // (that would close it, and the click would open it again).
        onMouseDown={e => {
          if (open) e.preventDefault()
        }}
        onClick={() => (open ? close(true) : openPicker())}
        onKeyDown={onTriggerKeyDown}
      >
        {leading}
        <span className="truncate font-medium text-text-secondary" data-model-name="">
          {label || t('picker.noModel')}
        </span>
        <span className="shrink-0 flex text-text-tertiary" title={providerName} aria-hidden="true">
          <ProviderGlyph id={provider} icon={threadProvider?.icon} />
        </span>
        {sharedBadge}
        <ChevronDown size={12} className="shrink-0 text-text-tertiary" aria-hidden="true" />
      </button>

      {open && (
        <div
          ref={dialogRef}
          id={dialogId}
          role="dialog"
          aria-label={t('picker.dialog')}
          tabIndex={-1}
          className="fixed z-overlay flex flex-col w-[min(22rem,calc(100vw-1rem))] max-h-[min(30rem,80vh)] bg-surface-2 border border-border-default rounded-lg shadow-lg animate-fade-in outline-hidden"
          style={{top: position.top, left: position.left}}
          onKeyDown={onDialogKeyDown}
          onBlur={onDialogBlur}
        >
          {(providers.length > 1 || searchable) && (
            <div className="flex flex-col gap-2 p-2 border-b border-border-subtle">
              {providers.length > 1 && (
                <div className="flex items-center gap-2">
                  <label htmlFor={providerSelectId} className="shrink-0 text-2xs font-medium text-text-tertiary">
                    {t('picker.provider')}
                  </label>
                  <Select
                    id={providerSelectId}
                    className="flex-1 min-w-0"
                    value={current?.id ?? ''}
                    onChange={e => {
                      setShownProvider(e.target.value)
                      setQuery('')
                      setActiveId(e.target.value === provider && model ? model : null)
                    }}
                    options={providers.map(p => ({value: p.id, label: p.name}))}
                  />
                </div>
              )}
              {searchable && (
                <div className="relative flex items-center">
                  <Search size={12} className="absolute left-2 text-text-tertiary" aria-hidden="true" />
                  <input
                    ref={searchRef}
                    type="text"
                    role="combobox"
                    aria-expanded="true"
                    aria-controls={listId}
                    aria-autocomplete="list"
                    aria-activedescendant={activeDescendant}
                    aria-label={t('picker.search')}
                    placeholder={t('picker.searchPlaceholder')}
                    autoComplete="off"
                    spellCheck={false}
                    value={query}
                    onChange={e => {
                      setQuery(e.target.value)
                      setActiveId(null)
                    }}
                    onKeyDown={onSearchKeyDown}
                    className="w-full h-8 pl-7 pr-2 rounded-md bg-surface-1 border border-border-default text-xs text-text-primary placeholder:text-text-tertiary outline-hidden focus:border-border-focus"
                  />
                </div>
              )}
            </div>
          )}

          {pickable.length === 0 ? (
            <div className="flex flex-col items-start gap-2 p-3 text-xs text-text-secondary">
              <p>
                {isAzure
                  ? t('picker.emptyAzure')
                  : t('picker.emptyProvider', {provider: current?.name ?? providerName})}
              </p>
              {isAzure && (
                <button
                  type="button"
                  className="min-h-6 font-medium text-brand-400 hover:text-brand-300 transition-colors"
                  onClick={manage}
                >
                  {t('picker.addModels')}
                </button>
              )}
            </div>
          ) : (
            <>
              {searchable && (
                <p
                  role="status"
                  className={clsx('text-xs text-text-tertiary', options.length === 0 ? 'p-3' : 'sr-only')}
                >
                  {options.length === 0 ? t('picker.noMatches', {query: query.trim()}) : ''}
                </p>
              )}
              <div
                ref={listRef}
                id={listId}
                role="listbox"
                aria-label={t('picker.list')}
                aria-activedescendant={activeDescendant}
                tabIndex={0}
                className={clsx(
                  'min-h-0 overflow-y-auto custom-scrollbar py-1 outline-hidden focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-brand-500',
                  options.length === 0 && 'hidden',
                )}
                onKeyDown={onListKeyDown}
                onFocus={onListFocus}
              >
                {groups.map((group, groupIndex) => {
                  // By position: a custom purpose's key is free text.
                  const groupHeadingId = `${baseId}-group-${groupIndex}`
                  return (
                    <div key={group.key} role="group" aria-labelledby={groupHeadingId}>
                      <div
                        id={groupHeadingId}
                        aria-hidden="true"
                        className="px-2 pt-2 pb-1 text-2xs font-semibold uppercase tracking-wider text-text-tertiary"
                      >
                        {group.label}
                      </div>
                      {group.models.map(m => {
                        const index = indexOf.get(m) ?? -1
                        const id = optionId(index)
                        const reasonKey = statusKey(m.status)
                        const kind = modelKind(m.id, m)
                        const isCurrent = current?.id === provider && m.id === model
                        const isDefault = defaults[current?.id ?? ''] === m.id
                        const contextWindow = m.contextLimit > 0 ? formatContextWindow(m.contextLimit) : ''
                        const detail = reasonKey
                          ? t(reasonKey)
                          : isRegistryEntry(m)
                            ? m.description
                            : [
                                m.inputCostPerM > 0
                                  ? t('picker.price', {input: m.inputCostPerM, output: m.outputCostPerM})
                                  : '',
                                m.supportsTools === false ? t('provider.noNativeTools') : '',
                              ]
                                .filter(Boolean)
                                .join(' · ')
                        const detailId = detail ? `${id}-detail` : undefined
                        return (
                          <div
                            key={m.id}
                            id={id}
                            role="option"
                            aria-selected={isCurrent}
                            aria-disabled={reasonKey ? true : undefined}
                            aria-describedby={detailId}
                            className={clsx(
                              'flex flex-col gap-0.5 mx-1 px-2 py-1.5 min-h-8 rounded-md border-l-2 text-xs',
                              index === activeIndex ? 'bg-surface-3 border-brand-500' : 'border-transparent',
                              reasonKey ? 'cursor-not-allowed text-text-tertiary' : 'cursor-pointer text-text-primary',
                            )}
                            // Keeps focus in the search box or the list.
                            onMouseDown={e => e.preventDefault()}
                            onMouseMove={() => {
                              if (activeId !== m.id) setActiveId(m.id)
                            }}
                            onClick={() => choose(m)}
                          >
                            {/* The {' '} separators keep the option's name
                                readable ("Docs writer Claude 200k context
                                window"); a flex row ignores them. */}
                            <div className="flex items-center gap-1.5 min-w-0">
                              <Check
                                size={12}
                                aria-hidden="true"
                                className={clsx('shrink-0 text-brand-400', !isCurrent && 'invisible')}
                              />
                              <span className="truncate font-medium">{m.displayName || m.id}</span>
                              {isDefault && (
                                <>
                                  {' '}
                                  <Badge
                                    tone="brand"
                                    srLabel={t('modelBadge.defaultSr')}
                                    title={t('modelBadge.defaultSr')}
                                  >
                                    {t('modelBadge.default')}
                                  </Badge>
                                </>
                              )}
                              {(kind === 'claude' || kind === 'agent') && (
                                <>
                                  {' '}
                                  <Badge tone="info">{tc(modelKindLabelKey(kind))}</Badge>
                                </>
                              )}
                              {m.shared && (
                                <>
                                  {' '}
                                  <Badge tone="neutral" icon={Users} srLabel={t('modelBadge.sharedSr', {org})}>
                                    {t('modelBadge.shared')}
                                  </Badge>
                                </>
                              )}
                              {contextWindow && (
                                <>
                                  {' '}
                                  <span className="ml-auto shrink-0 pl-2 text-2xs tabular-nums text-text-tertiary">
                                    <span aria-hidden="true">{contextWindow}</span>
                                    <span className="sr-only">
                                      {t('picker.contextWindow', {window: contextWindow})}
                                    </span>
                                  </span>
                                </>
                              )}
                            </div>
                            {detail && (
                              // aria-hidden keeps line 2 out of the option's
                              // NAME; aria-describedby still reads it.
                              <div
                                id={detailId}
                                aria-hidden="true"
                                className="pl-[18px] text-2xs text-text-tertiary line-clamp-2 wrap-anywhere"
                              >
                                {detail}
                              </div>
                            )}
                          </div>
                        )
                      })}
                    </div>
                  )
                })}
              </div>
            </>
          )}

          <div className="flex flex-col border-t border-border-subtle py-1">
            {reviewPrompt && isAzure && (
              <div className="flex items-center gap-2 px-3 py-1.5 text-xs text-text-secondary">
                <span className="min-w-0 flex-1">{t('picker.reviewPrompt')}</span>
                <button
                  type="button"
                  className="shrink-0 min-h-6 font-medium text-brand-400 hover:text-brand-300 transition-colors"
                  onClick={manage}
                >
                  {t('picker.review')}
                </button>
              </div>
            )}
            <button
              type="button"
              className="flex items-center gap-2 mx-1 px-2 min-h-8 rounded-md text-xs text-text-primary text-left hover:bg-surface-3 transition-colors"
              onClick={manage}
            >
              <Settings size={14} aria-hidden="true" className="shrink-0 text-text-tertiary" />
              {t('picker.manage')}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
