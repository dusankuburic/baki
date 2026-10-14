import {useEffect, useState} from 'react'
import clsx from 'clsx'
import {userInitials, userColor} from '@/lib/avatar'
import {isInternalAvatarUrl, loadAvatar, onAvatarsChanged, resolveAvatarUrl} from '@/lib/avatarCache'

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

// An uploaded avatar's URL is the authenticated serving route, which an <img> tag cannot call: internal paths
// render from the avatar cache (a bearer-fetched object URL) and fall back to the initials until it arrives.
export default function Avatar({name, colorSeed, avatarUrl, size = 'md', className}: AvatarProps) {
  const [imgFailed, setImgFailed] = useState(false)

  const [lastUrl, setLastUrl] = useState(avatarUrl)
  if (avatarUrl !== lastUrl) {
    setLastUrl(avatarUrl)
    setImgFailed(false)
  }

  const internal = isInternalAvatarUrl(avatarUrl)
  const [objectUrl, setObjectUrl] = useState<string | null>(() => resolveAvatarUrl(avatarUrl))
  useEffect(() => {
    if (!internal) return
    let alive = true
    void loadAvatar(avatarUrl!).then(url => {
      if (alive) setObjectUrl(url)
    })
    return () => {
      alive = false
    }
  }, [internal, avatarUrl])
  // Another surface invalidated or re-fetched the avatar while this one is mounted.
  useEffect(() => (internal ? onAvatarsChanged(() => setObjectUrl(resolveAvatarUrl(avatarUrl))) : undefined), [internal, avatarUrl])

  const src = internal ? objectUrl : avatarUrl
  const showImage = !!src && !imgFailed

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
        <img src={src} alt={name} className="w-full h-full object-cover" onError={() => setImgFailed(true)} />
      ) : (
        userInitials(name)
      )}
    </div>
  )
}
