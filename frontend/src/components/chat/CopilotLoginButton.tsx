import {providersApi} from '@/api'
import DeviceAuthLoginButton, {type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const copilotProvider: DeviceAuthProvider = {
  name: 'Copilot',
  label: 'Sign in with GitHub',
  buttonClassName: 'bg-[#6e40c9] hover:bg-[#5a32a3]',
  getUser: () => providersApi.getCopilotUser(),
  startAuth: () => providersApi.startCopilotAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollCopilotAuth(deviceCode),
  revokeAuth: () => providersApi.revokeCopilotAuth(),
}

export default function CopilotLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={copilotProvider} onAuthComplete={onAuthComplete} />
}
