import {providersApi} from '@/api'
import i18n from '@/i18n'
import DeviceAuthLoginButton, {type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const copilotProvider: DeviceAuthProvider = {
  name: 'Copilot',
  // `name` is a provider IDENTIFIER (matched against the API's provider id),
  // not copy — only `label` is user-facing.
  label: i18n.t('chat:auth.signInWithGitHub'),
  buttonClassName: 'bg-[#6e40c9] hover:bg-[#5a32a3]',
  getUser: () => providersApi.getCopilotUser(),
  startAuth: () => providersApi.startCopilotAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollCopilotAuth(deviceCode),
  revokeAuth: () => providersApi.revokeCopilotAuth(),
}

export default function CopilotLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={copilotProvider} onAuthComplete={onAuthComplete} />
}
