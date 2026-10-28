import {useCallback, useId, useMemo, useRef, useState} from 'react'
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

interface Stored {
  stored: string[]
  // The ai section's revision, the section write's precondition; null from a replica without the section route.
  revision: string | null
}

interface Loaded extends Stored {
  // The catalogue's choices, and those plus the stored ids it does not list.
  offered: Choice[]
  choices: Choice[]
}

interface AISection {
  allowedProviders?: string[] | null
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

// A replica from before the section route (R-config-4) answers the router's own 404, which carries no error code
// (or a 405 behind a proxy that knows the path): the section falls back to the whole-document write there. A 404
// the route itself answers always carries a code (render.Error names every status), and is an error like any other.
function noSectionRoute(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 405 || (err.status === 404 && !err.code))
}

// The choices: the catalogue's, then each stored id it does not list, which stays visible (and kept) until an admin
// unchecks it.
function choicesFor(offered: Choice[], stored: string[]): Choice[] {
  const ids = new Set(offered.map(c => c.id))
  return [...offered, ...[...new Set(stored)].filter(id => !ids.has(id)).map(id => ({id}))]
}

// The stored list and the ai section's revision; from a replica without the section route, the list alone.
async function loadStored(orgId: string): Promise<Stored> {
  try {
    const section = await settingsApi.getOrgSettingsSection<AISection>(orgId, 'ai')
    return {stored: section.values.allowedProviders ?? [], revision: section.revision}
  } catch (err) {
    if (!noSectionRoute(err)) throw err
  }
  const doc = await settingsApi.getOrgSettings(orgId)
  return {stored: doc.ai?.allowedProviders ?? [], revision: null}
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  const sa = new Set(a)
  const sb = new Set(b)
  return sa.size === sb.size && [...sa].every(id => sb.has(id))
}

/** The org settings' ai.allowedProviders (ADR 0028), for org admins. The route keeps the stored list when a
 * write leaves the field out (the Rules panel's partial writes) and clears it on an explicit []; so this
 * section is the only writer that names the field, and only when the admin changed the list. It writes through the
 * section route (PATCH .../sections/ai, R-config-4): the server merges the list into the ai section as stored, so
 * another admin's concurrent change to another field is kept, and the revision it loaded turns a write over a list
 * that changed since into a 409, after which the list is reloaded before anything is saved again. */
export default function OrgAIProvidersSection({orgId}: {orgId: string}) {
  const {t} = useTranslation('settings')
  const titleId = useId()
  const {data, isLoading, error, setData} = useAsync<Loaded | null>(async () => {
    const [catalog, {stored, revision}] = await Promise.all([loadCatalog(), loadStored(orgId)])
    if (!catalog) return null
    const offered = catalog.map(p => ({id: p.id, name: p.name}))
    return {offered, choices: choicesFor(offered, stored), stored, revision}
  }, [orgId])
  // A reload after a stale save (the form's own, so a failed one leaves the form and its selection in place): the
  // new list and revision re-key the form, which remounts with them.
  const reloaded = useCallback(
    (fresh: Stored) => setData(d => d && {...d, ...fresh, choices: choicesFor(d.offered, fresh.stored)}),
    [setData],
  )

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
          key={[orgId, data.revision ?? '', ...data.stored].join('\n')}
          orgId={orgId}
          titleId={titleId}
          choices={data.choices}
          stored={data.stored}
          revision={data.revision}
          onReloaded={reloaded}
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
  revision: loadedRevision,
  onReloaded,
}: {
  orgId: string
  titleId: string
  choices: Choice[]
  stored: string[]
  revision: string | null
  onReloaded: (fresh: Stored) => void
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const errorId = useId()
  const groupRef = useRef<HTMLDivElement>(null)
  // The list as the server holds it: loaded, then as saved. Held here, not in the parent's data, whose change
  // re-keys (remounts) the form and would drop the save button's focus to the page.
  const [stored, setStored] = useState(loaded)
  // The ai section's revision as last read or written; null writes the whole document (a replica without the route).
  const [revision, setRevision] = useState(loadedRevision)
  const [limited, setLimited] = useState(stored.length > 0)
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set(stored))
  const [serverError, setServerError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // The reload after a stale save failed: the selection stays, and the admin can try the reload again.
  const [reloadFailed, setReloadFailed] = useState(false)
  const [reloading, setReloading] = useState(false)

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

  // The section write, naming only the list, with the revision last read; false when the replica has no section
  // route, and the whole-document write is the caller's.
  const saveSection = async (): Promise<boolean> => {
    if (revision === null) return false
    try {
      const out = await settingsApi.patchOrgSettingsSection<AISection>(orgId, 'ai', {allowedProviders: next}, revision)
      setRevision(out.revision)
      return true
    } catch (err) {
      if (noSectionRoute(err)) return false
      throw err
    }
  }

  // Reloads the stored list after a stale save. Only a reload that succeeded says the current list is loaded (the
  // form then remounts with it); a failed one keeps the admin's selection and offers the reload again.
  const reload = async () => {
    if (reloading) return
    setReloading(true)
    try {
      const fresh = await loadStored(orgId)
      setReloadFailed(false)
      toast.error(t('orgAiProviders.stale'))
      onReloaded(fresh)
    } catch {
      setReloadFailed(true)
    } finally {
      setReloading(false)
    }
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
      if (!(await saveSection())) {
        // A replica without the section route: post the stored document as it is now (read past the client's GET
        // cache), with only the list changed. The current route keeps the sections a body leaves out, but a replica
        // from before that change replaces the document whole, so a bare {ai} would wipe the rule overrides there
        // during a rolling deploy. The section write never reaches such a replica as a write: it answers 404.
        const fresh = await settingsApi.getOrgSettings(orgId, {noDedup: true})
        await settingsApi.updateOrgSettings(orgId, {...fresh, ai: {...fresh.ai, allowedProviders: next}})
      }
      toast.success(t('orgAiProviders.saved'))
      setStored(next)
    } catch (err) {
      if (getErrorCode(err) === 'PROVIDER_UNKNOWN') refuseUnknown(err)
      else if (getErrorCode(err) === 'SETTINGS_STALE') {
        // The ai section changed since it was loaded (the revision covers the whole section, so it may be another
        // field): reload the list and let the admin choose again, rather than writing this list over one they have
        // not seen.
        await reload()
      } else toast.error(t('orgAiProviders.saveFailed'), {description: getErrorMessage(err)})
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
      {reloadFailed && (
        <div role="alert" className="flex flex-wrap items-center gap-2 text-2xs text-semantic-error">
          <span>{t('orgAiProviders.staleReloadFailed')}</span>
          <Button
            size="sm"
            variant="ghost"
            onClick={reload}
            aria-disabled={reloading || undefined}
            className={clsx(reloading && 'cursor-not-allowed opacity-50')}
          >
            {t('orgAiProviders.reload')}
          </Button>
        </div>
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
