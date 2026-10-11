import {providersApi} from '@/api'
import i18n from '@/i18n'
import DeviceAuthLoginButton, {DEVICE_BUTTON_CLASS, type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const githubProvider: DeviceAuthProvider = {
  name: 'GitHub',
  label: i18n.t('chat:auth.signInWithGitHub'),
  buttonClassName: DEVICE_BUTTON_CLASS['github-models']!,
  getUser: () => providersApi.getGitHubUser(),
  startAuth: () => providersApi.startGitHubAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollGitHubAuth(deviceCode),
  revokeAuth: () => providersApi.revokeGitHubAuth(),
}

export default function GitHubLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={githubProvider} onAuthComplete={onAuthComplete} />
}
