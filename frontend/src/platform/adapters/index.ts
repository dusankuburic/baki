/**
 * Platform adapter factory
 */

import { isTauri } from '../guards';
import type { PlatformAdapter } from '../types';
import { TauriAdapter } from './TauriAdapter';
import { WebAdapter } from './WebAdapter';

/**
 * Create appropriate platform adapter based on current environment
 */
export function createAdapter(): PlatformAdapter {
  if (isTauri()) {
    return new TauriAdapter();
  } else {
    return new WebAdapter();
  }
}

// Re-export guards so consumers can import from a single location
export { getPlatformType, getPlatformCapabilities } from '../guards';

// Export adapters for direct usage if needed
export { TauriAdapter } from './TauriAdapter';
export { WebAdapter } from './WebAdapter';