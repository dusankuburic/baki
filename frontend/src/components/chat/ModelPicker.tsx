import {useTranslation} from 'react-i18next'
import {useCallback, useEffect, useId, useMemo, useRef, useState} from 'react'
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
import {PROVIDER_GLYPHS, hasProviderGlyph} from './providerGlyphs'

export interface PickerProvider {
  id: string
  name: string
  shared?: boolean
  icon?: string
  models: ModelDetail[]
}

interface Props {
  providers: PickerProvider[]
  provider: string
  model: string
  label: string
  modelDetail?: ModelDetail
  defaults: Record<string, string>
  onSelect: (providerId: string, model: ModelDetail) => void
  orgName?: string
  reviewPrompt?: boolean
  disabled?: boolean
  leading?: React.ReactNode
  title?: string
  className?: string
  triggerRef?: React.MutableRefObject<HTMLButtonElement | null>
}

export const PICKER_SEARCH_THRESHOLD = 7
export const AZURE_ACCOUNTS_ANCHOR = AZURE_SECTION_ANCHOR

type StatusKey =
  | 'modelStatus.needs_credentials'
  | 'modelStatus.unreadable'
  | 'modelStatus.invalid_config'
  | 'modelStatus.unsupported'
  | 'modelStatus.unknown'

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

function ProviderGlyph({id, icon}: {id: string; icon?: string}) {
  if (hasProviderGlyph(icon)) {
    const Icon = PROVIDER_GLYPHS[icon]!
    return <Icon size={12} />
  }
  if (id === 'local') return <HardDrive size={12} />
  if (id === 'github-models' || id === 'copilot') return <GithubMark size={12} />
  return <Cloud size={12} />
}

interface GroupView {
  key: string
  label: string
  models: ModelDetail[]
}

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
  const [shownProvider, setShownProvider] = useState(provider)
  const [query, setQuery] = useState('')
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
  const indexOf = useMemo(() => new Map(options.map((m, i) => [m, i])), [options])
  const activeIndex = activeId === null ? -1 : options.findIndex(m => m.id === activeId)
  const optionId = (index: number) => `${baseId}-option-${index}`
  const activeDescendant = activeIndex >= 0 ? optionId(activeIndex) : undefined

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
    const own = providers.some(p => p.id === provider)
    setShownProvider(own ? provider : (providers[0]?.id ?? provider))
    setActiveId(own && model ? model : null)
    setQuery('')
    setOpen(true)
  }, [disabled, providers, provider, model])

  useEffect(() => {
    if (!open) return
    const dialog = dialogRef.current
    const target =
      searchRef.current ?? listRef.current ?? dialog?.querySelector<HTMLElement>('select, button') ?? dialog
    target?.focus({preventScroll: true})
  }, [open])

  const {update: reposition} = position
  useEffect(() => {
    if (open) reposition()
  }, [open, reposition, groups, shownProvider])

  useEffect(() => {
    if (!open || !activeDescendant) return
    document.getElementById(activeDescendant)?.scrollIntoView({block: 'nearest'})
  }, [open, activeDescendant])

  useEffect(
    () =>
      useChatStore.subscribe((state, prev) => {
        if (state.modelPickerRequest > prev.modelPickerRequest) openPicker()
      }),
    [openPicker],
  )

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
    e.preventDefault()
    e.stopPropagation()
    if (searchable && query) {
      setQuery('')
      setActiveId(null)
      return
    }
    close(true)
  }

  const choose = (m: ModelDetail) => {
    if (statusKey(m.status)) return
    onSelect(current?.id ?? shownProvider, m)
    close(true)
  }

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

  const threadProvider = providers.find(p => p.id === provider)
  const providerName = threadProvider?.name || (isKnownProviderId(provider) ? PROVIDERS[provider].label : provider)
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
                            onMouseDown={e => e.preventDefault()}
                            onMouseMove={() => {
                              if (activeId !== m.id) setActiveId(m.id)
                            }}
                            onClick={() => choose(m)}
                          >
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
