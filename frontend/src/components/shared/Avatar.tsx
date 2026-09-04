import {useState} from 'react'
import clsx from 'clsx'
import {userInitials, userColor} from '@/lib/avatar'

const SIZE_CLASSES = {
  sm: 'w-7 h-7 text-2xs',
  md: 'w-8 h-8 text-xs',
  lg: 'w-11 h-11 text-lg',
} as const

export type AvatarSize = keyof typeof SIZE_CLASSES

interface AvatarProps {
  /** Display name used for initials and the color hash fallback when colorSeed is omitted. */
  name: string
  /** Stable identifier (e.g. user id) to hash into a color; falls back to `name` when absent. */
  colorSeed?: string
  avatarUrl?: string
  size?: AvatarSize
  className?: string
}

// Avatar renders a user's avatarUrl image, falling back to initials-on-color
// when there is no URL or the image fails to load (broken link, revoked
// signed URL, offline) — every existing avatar usage in the app rendered a
// bare <img> with no error handling, so a dead URL showed a broken-image icon
// instead of the initials fallback it already had the data for.
export default function Avatar({name, colorSeed, avatarUrl, size = 'md', className}: AvatarProps) {
  const [imgFailed, setImgFailed] = useState(false)

  // A failed load only condemns the URL that failed — when avatarUrl changes
  // (e.g. the user is typing a new one in the profile form), try again.
  // Adjusted DURING render rather than in an effect: React re-runs this
  // component before committing, so the reset never reaches the DOM as an
  // extra painted frame the way an effect-then-rerender would.
  const [lastUrl, setLastUrl] = useState(avatarUrl)
  if (avatarUrl !== lastUrl) {
    setLastUrl(avatarUrl)
    setImgFailed(false)
  }

  const showImage = !!avatarUrl && !imgFailed

  return (
    <div
      className={clsx(
        'rounded-full flex-shrink-0 flex items-center justify-center font-semibold text-white overflow-hidden select-none',
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
