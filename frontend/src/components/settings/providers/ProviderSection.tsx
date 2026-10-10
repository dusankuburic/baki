import {isKnownProviderId, type ProviderSpec} from '@/types'
import CredentialForm from './CredentialForm'
import {PROVIDER_OVERRIDES} from './overrides'

export interface ProviderSectionProps {
  spec: ProviderSpec
  /** A credential changed (saved, removed, signed in or out): the host re-checks the providers. */
  onConfigured: () => void
  /**
   * The provider became usable: a sign-in completed (`always` makes it the
   * active provider) or its first model was added (`ifUnconfigured`: only
   * when the current active provider cannot chat).
   */
  onActivated: (id: string, when: 'always' | 'ifUnconfigured') => void
  /** The server says the provider is not offered here (the local model on a cloud server): the host drops the section. */
  onUnavailable: (id: string) => void
}

// A provider's block under an h3 (the panel's title is the Settings h2).
// The heading's id, provider-<id>, is a Settings deep-link anchor; it takes
// focus programmatically only (tabIndex -1).
export function ProviderHeading({
  id,
  name,
  color,
  children,
}: {
  id: string
  name: string
  /** The dot's colour; the heading's own colour without one. */
  color?: string
  children: React.ReactNode
}) {
  return (
    <section data-provider={id}>
      <div className="flex items-center gap-2 mb-3">
        <span aria-hidden="true" className="w-3 h-3 rounded-full" style={{backgroundColor: color || 'currentColor'}} />
        <h3 id={`provider-${id}`} tabIndex={-1} className="text-base font-medium text-text-primary">
          {name}
        </h3>
      </div>
      {children}
    </section>
  )
}

// One catalogue entry: the heading, then the bespoke panel the bundle keeps
// for the provider (overrides.tsx) or the credential form its schema asks for.
export default function ProviderSection(props: ProviderSectionProps) {
  const {spec, onConfigured, onActivated} = props
  const Override = isKnownProviderId(spec.id) ? PROVIDER_OVERRIDES[spec.id] : undefined
  return (
    <ProviderHeading id={spec.id} name={spec.name} color={spec.color}>
      {Override ? (
        <Override {...props} />
      ) : (
        <CredentialForm spec={spec} onConfigured={onConfigured} onSignedIn={() => onActivated(spec.id, 'always')} />
      )}
    </ProviderHeading>
  )
}
