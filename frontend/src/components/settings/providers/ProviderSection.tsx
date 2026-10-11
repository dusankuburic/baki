import {isKnownProviderId, type ProviderSpec} from '@/types'
import CredentialForm from './CredentialForm'
import {PROVIDER_OVERRIDES} from './overrides'

export interface ProviderSectionProps {
  spec: ProviderSpec
  onConfigured: () => void
  onActivated: (id: string, when: 'always' | 'ifUnconfigured') => void
  onUnavailable: (id: string) => void
}

export function ProviderHeading({
  id,
  name,
  color,
  children,
}: {
  id: string
  name: string
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
