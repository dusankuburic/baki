import {useState} from 'react'
import clsx from 'clsx'
import {userInitials, userColor} from '@/lib/avatar'

const SIZE_CLASSES = {
  sm: 'w-7 h-7 text-2xs',
  md: 'w-8 h-8 text-xs',
  lg: 'w-11 h-11 text-lg',
  xl: 'w-16 h-16 text-xl',
} as const

export type AvatarSize = keyof typeof SIZE_CLASSES

interface AvatarProps {
  name: string
  colorSeed?: string
  avatarUrl?: string
  size?: AvatarSize
  className?: string
}

export default function Avatar({name, colorSeed, avatarUrl, size = 'md', className}: AvatarProps) {
  const [imgFailed, setImgFailed] = useState(false)

  const [lastUrl, setLastUrl] = useState(avatarUrl)
  if (avatarUrl !== lastUrl) {
    setLastUrl(avatarUrl)
    setImgFailed(false)
  }

  const showImage = !!avatarUrl && !imgFailed

  return (
    <div
      className={clsx(
        'rounded-full shrink-0 flex items-center justify-center font-semibold text-white overflow-hidden select-none',
        SIZE_CLASSES[size],
        className,
      )}
      style={showImage ? undefined : {background: userColor(colorSeed || name)}}
    >
      {showImage ? (
        <img src={avatarUrl} alt={name} className="w-full h-full object-cover" onError={() => setImgFailed(true)} />
      ) : (
        userInitials(name)
      )}
    </div>
  )
}
