import {logger} from '@/lib/logger'

// The desktop backend died and a new process took its place (R-frontend-c7): the api client has moved to the new
// port and token, and whatever was tied to the old process (a chat stream it was running) is over. Kept apart from
// the api client so the code that reacts to it does not depend on how the client is mocked.

type Listener = () => void

const listeners = new Set<Listener>()

/** Calls cb each time the backend is replaced by a new process. Returns the unsubscribe function. */
export function onBackendRestarted(cb: Listener): () => void {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

export function notifyBackendRestarted(): void {
  for (const cb of [...listeners]) {
    try {
      cb()
    } catch (err) {
      logger.error('Backend restart listener threw (isolated)', err)
    }
  }
}
