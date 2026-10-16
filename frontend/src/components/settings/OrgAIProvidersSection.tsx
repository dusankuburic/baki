import {useId, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {providersApi, settingsApi} from '@/api'
import {ApiError} from '@/api/client'
import {Button, Checkbox, Switch, useToast} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {useFormValidation} from '@/hooks/useFormValidation'
import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import type {ProviderSpec} from '@/types'

interface Choice {
  id: string
  name?: string
}

interface Loaded {
  choices: Choice[]
  stored: string[]
}

const FIELDS = ['providers'] as const

// A server without the catalogue predates the allowlist (Phase 4b): there is nothing to edit.
async function loadCatalog(): Promise<ProviderSpec[] | null> {
  try {
    return await providersApi.getProviderCatalog()
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null
    throw err
  }
}

// The ids a PROVIDER_UNKNOWN refusal names (details.providers); null from a server that sends none.
function refusedIds(err: unknown): string[] | null {
  const ids = err instanceof ApiError ? err.details?.providers : undefined
  return Array.isArray(ids) ? ids.filter((id): id is string => typeof id === 'string') : null
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  const sa = new Set(a)
  const sb = new Set(b)
  return sa.size === sb.size && [...sa].every(id => sb.has(id))
}

/** The org settings' ai.allowedProviders (ADR 0028), for org admins. The route keeps the stored list when a
 * write leaves the field out (the Rules panel's partial writes) and clears it on an explicit []; so this
 * section is the only writer that names the field, and only when the admin changed the list. */
export default function OrgAIProvidersSection({orgId}: {orgId: string}) {
  const {t} = useTranslation('settings')
  const titleId = useId()
  const {data, isLoading, error} = useAsync<Loaded | null>(async () => {
    const [catalog, doc] = await Promise.all([loadCatalog(), settingsApi.getOrgSettings(orgId)])
    if (!catalog) return null
    const stored = doc.ai?.allowedProviders ?? []
    // A stored id the catalogue does not list stays visible (and kept) until an admin unchecks it.
    const offered = new Set(catalog.map(p => p.id))
    const choices: Choice[] = [
      ...catalog.map(p => ({id: p.id, name: p.name})),
      ...[...new Set(stored)].filter(id => !offered.has(id)).map(id => ({id})),
    ]
    return {choices, stored}
  }, [orgId])

  if (!isLoading && !error && !data) return null

  return (
    <section className="space-y-2">
      <div>
        <h4 id={titleId} className="text-sm font-medium">
          {t('orgAiProviders.title')}
        </h4>
        <p className="text-2xs text-text-tertiary">{t('orgAiProviders.subtitle')}</p>
      </div>
      {error ? (
        <p className="text-2xs text-semantic-error">{t('orgAiProviders.loadFailed')}</p>
      ) : !data ? (
        <p className="text-2xs text-text-tertiary">{t('orgAiProviders.loading')}</p>
      ) : (
        <AllowlistForm
          key={[orgId, ...data.stored].join('\n')}
          orgId={orgId}
          titleId={titleId}
          choices={data.choices}
          stored={data.stored}
        />
      )}
    </section>
  )
}

function AllowlistForm({
  orgId,
  titleId,
  choices,
  stored: loaded,
}: {
  orgId: string
  titleId: string
  choices: Choice[]
  stored: string[]
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const errorId = useId()
  const groupRef = useRef<HTMLDivElement>(null)
  // The list as the server holds it: loaded, then as saved. Held here, not in the parent's data, whose change
  // re-keys (remounts) the form and would drop the save button's focus to the page.
  const [stored, setStored] = useState(loaded)
  const [limited, setLimited] = useState(stored.length > 0)
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set(stored))
  const [serverError, setServerError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  const next = useMemo(
    () => (limited ? choices.map(c => c.id).filter(id => selected.has(id)) : []),
    [choices, limited, selected],
  )
  const changed = limited !== stored.length > 0 || (limited && !sameSet(next, stored))

  const v = useFormValidation({
    fields: FIELDS,
    validate: () => (limited && next.length === 0 ? {providers: t('orgAiProviders.noneSelected')} : {}),
  })
  const fieldError = serverError ?? v.errorFor('providers')
  const blocked = !changed || saving

  // Focus a checkbox of the group, by choice index (the first when the id is not listed).
  const focusChoice = (id?: string) => {
    const boxes = groupRef.current?.querySelectorAll<HTMLElement>('[role="checkbox"]')
    const index = choices.findIndex(c => c.id === id)
    boxes?.[Math.max(0, index)]?.focus()
  }

  // A newer server names the ids it does not know; an older one does not, and the likely ones are the selected
  // ids its catalogue does not offer. Unchecking them lets the admin save the rest, and the message says which.
  const refuseUnknown = (err: unknown) => {
    const named = refusedIds(err) ?? choices.filter(c => !c.name).map(c => c.id)
    const ids = named.filter(id => selected.has(id))
    if (ids.length === 0) {
      setServerError(t('orgAiProviders.unknownProvider'))
      focusChoice()
      return
    }
    setSelected(prev => new Set([...prev].filter(id => !ids.includes(id))))
    const names = ids.map(id => choices.find(c => c.id === id)?.name ?? id)
    setServerError(t('orgAiProviders.unknownProviders', {count: ids.length, ids: names.join(', ')}))
    focusChoice(ids[0])
  }

  const toggle = (id: string, on: boolean) => {
    setServerError(null)
    setSelected(prev => {
      const out = new Set(prev)
      if (on) out.add(id)
      else out.delete(id)
      return out
    })
  }

  const save = async () => {
    if (blocked) return
    if (v.submitValidate()) {
      focusChoice()
      return
    }
    setSaving(true)
    setServerError(null)
    try {
      // Post the stored document as it is now (read past the client's GET cache), with only the list changed.
      // The current route keeps the sections a body leaves out, but a replica from before that change replaces
      // the document whole, so a bare {ai} would wipe the rule overrides there during a rolling deploy.
      const fresh = await settingsApi.getOrgSettings(orgId, {noDedup: true})
      await settingsApi.updateOrgSettings(orgId, {...fresh, ai: {...fresh.ai, allowedProviders: next}})
      toast.success(t('orgAiProviders.saved'))
      setStored(next)
    } catch (err) {
      if (getErrorCode(err) === 'PROVIDER_UNKNOWN') refuseUnknown(err)
      else toast.error(t('orgAiProviders.saveFailed'), {description: getErrorMessage(err)})
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="space-y-2">
      <Switch
        checked={limited}
        onChange={on => {
          setServerError(null)
          setLimited(on)
        }}
        label={t('orgAiProviders.limit')}
      />
      {limited ? (
        <div
          ref={groupRef}
          role="group"
          aria-labelledby={titleId}
          aria-describedby={fieldError ? errorId : undefined}
          onBlur={() => v.touch('providers')}
          className="flex flex-wrap gap-x-4 gap-y-1.5"
        >
          {choices.map(c => (
            <Checkbox
              key={c.id}
              checked={selected.has(c.id)}
              onChange={on => toggle(c.id, on)}
              label={c.name ?? t('orgAiProviders.notOffered', {id: c.id})}
            />
          ))}
        </div>
      ) : (
        <p className="text-2xs text-text-tertiary">{t('orgAiProviders.allAllowed')}</p>
      )}
      {limited && fieldError && (
        <p id={errorId} role="alert" className="text-2xs text-semantic-error">
          {fieldError}
        </p>
      )}
      {/* aria-disabled, not disabled: a disabled button loses focus to the page while saving and once saved. */}
      <Button
        size="sm"
        onClick={save}
        aria-disabled={blocked || undefined}
        className={clsx(blocked && 'cursor-not-allowed opacity-50')}
      >
        {saving ? t('orgAiProviders.saving') : t('orgAiProviders.save')}
      </Button>
    </div>
  )
}
