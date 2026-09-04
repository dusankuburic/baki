import {useTranslation} from 'react-i18next'
type SpinnerProps = {
  size?: number
  color?: string
  className?: string
}

export default function Spinner({size = 16, color, className}: SpinnerProps) {
  const {t} = useTranslation()
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      className={className}
      style={color ? {color} : undefined}
      role="status"
      aria-label={t('loadingAria')}
    >
      <circle
        cx="8"
        cy="8"
        r="6.5"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeDasharray="28 12"
        className="animate-spin"
        style={{transformOrigin: 'center'}}
      />
    </svg>
  )
}
