import {providersApi} from '@/api'
import i18n from '@/i18n'
import DeviceAuthLoginButton, {DEVICE_BUTTON_CLASS, type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const copilotProvider: DeviceAuthProvider = {
  name: 'Copilot',
  label: i18n.t('chat:auth.signInWithGitHub'),
  buttonClassName: DEVICE_BUTTON_CLASS['copilot']!,
  getUser: () => providersApi.getCopilotUser(),
  startAuth: () => providersApi.startCopilotAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollCopilotAuth(deviceCode),
  revokeAuth: () => providersApi.revokeCopilotAuth(),
}

export default function CopilotLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={copilotProvider} onAuthComplete={onAuthComplete} />
}
