import {isTauri} from '@/platform/guards'
import {currentUserId} from '../storeRegistry'

/**
 * Whether the server keeps an index of this user's chat threads (R-frontend-c26 c): it lists and names the threads
 * of whoever is signed in. The desktop app stores conversations in files and a web deployment without accounts has
 * no owner to list them for — no one is signed in there — so neither has one, and the browser's own index is the
 * only list of the threads.
 */
export function serverThreadIndexAvailable(): boolean {
  return !isTauri() && currentUserId() !== null
}
