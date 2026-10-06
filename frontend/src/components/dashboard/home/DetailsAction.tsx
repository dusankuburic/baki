import {useTranslation} from 'react-i18next'
import {ArrowRight} from 'lucide-react'

// The "Details" link on a Home card: opens the Analytics page scrolled to the
// section that expands on the card. Only offered where analytics exist.
export function DetailsAction({title, onClick}: {title: string; onClick?: () => void}) {
  const {t} = useTranslation('dashboard')
  if (!onClick) return null
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={t('home.detailsOf', {title})}
      className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-2xs font-medium text-brand-400 transition-colors duration-fast hover:text-brand-300 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
    >
      {t('common.details')}
      <ArrowRight size={11} aria-hidden="true" />
    </button>
  )
}
