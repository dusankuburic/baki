
import {isTauri} from '../guards'
import type {PlatformAdapter} from '../types'
import {TauriAdapter} from './TauriAdapter'
import {WebAdapter} from './WebAdapter'

export function createAdapter(): PlatformAdapter {
  if (isTauri()) {
    return new TauriAdapter()
  } else {
    return new WebAdapter()
  }
}

export {getPlatformType, getPlatformCapabilities} from '../guards'

export {TauriAdapter} from './TauriAdapter'
export {WebAdapter} from './WebAdapter'
