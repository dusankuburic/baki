import {providersApi} from '@/api'
import i18n from '@/i18n'
import DeviceAuthLoginButton, {type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const githubProvider: DeviceAuthProvider = {
  name: 'GitHub',
  // `name` is a provider IDENTIFIER (matched against the API's provider id),
  // not copy — only `label` is user-facing.
  label: i18n.t('chat:auth.signInWithGitHub'),
  buttonClassName: 'bg-[#24292e] hover:bg-[#2f363d]',
  getUser: () => providersApi.getGitHubUser(),
  startAuth: () => providersApi.startGitHubAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollGitHubAuth(deviceCode),
  revokeAuth: () => providersApi.revokeGitHubAuth(),
}

export default function GitHubLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={githubProvider} onAuthComplete={onAuthComplete} />
}
