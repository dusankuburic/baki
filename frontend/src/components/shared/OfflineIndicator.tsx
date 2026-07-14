import {useState, useEffect} from 'react'
import {WifiOff} from 'lucide-react'

// OfflineIndicator shows a small banner at the bottom of the screen when the
// network connection drops. Listens to navigator.onLine + window online/offline
// events. Hidden in Tauri (always "online" — the sidecar runs locally).
export default function OfflineIndicator() {
  const [online, setOnline] = useState(typeof navigator !== 'undefined' ? navigator.onLine : true)

  useEffect(() => {
    const goOnline = () => setOnline(true)
    const goOffline = () => setOnline(false)
    window.addEventListener('online', goOnline)
    window.addEventListener('offline', goOffline)
    return () => {
      window.removeEventListener('online', goOnline)
      window.removeEventListener('offline', goOffline)
    }
  }, [])

  if (online) return null

  return (
    <div
      className="fixed bottom-0 left-0 right-0 z-40 bg-amber-500/95 text-amber-950 text-2xs font-medium px-4 py-1.5 flex items-center justify-center gap-1.5 animate-fade-in print:hidden"
      role="status"
      aria-live="polite"
    >
      <WifiOff size={12} />
      You're offline — showing cached data. Some actions are unavailable.
    </div>
  )
}
